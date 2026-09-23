# AWS deployment

Forge's reference cloud architecture uses an HTTPS Application Load Balancer,
ECS Fargate API and worker services, and an existing PostgreSQL database. The
[CloudFormation template](../deploy/aws/stack.yaml) provisions the execution
plane. The default enabled configuration is two API tasks and ten worker tasks
with 256 execution slots each. This is provisioned capacity, not a benchmark.

The deployment files are provided for a future rollout. No hosted endpoint or
AWS deployment is established by this repository. The console's cloud region is
configuration metadata; its simulated connection indicator is not an AWS health
check. Workers continue to use the included agent simulator.

## Prepare the account

Use a single AWS account and region for this reference deployment. Prepare:

1. A VPC with DNS support, two public subnets in different availability zones,
   and private application subnets. Public subnets need an internet-gateway route.
   Private tasks need outbound HTTPS through NAT, or ECR, S3, Secrets Manager,
   and CloudWatch Logs endpoints with the appropriate policies and routes.
2. A private RDS PostgreSQL 17 instance in that VPC, listening on port 5432.
   Create the `forge` database and a login that owns its application schema and
   can create migration tables. Configure backups and recovery independently.
   The template adds access from Forge's task security group to the database
   security group; it does not create or replace the database.
3. An issued ACM certificate in the same region for your intended hostname,
   such as `forge.your-domain.example`, and access to its DNS records.
4. AWS CLI v2, Docker with Linux image support, `curl`, `jq`, and an authenticated
   deployment identity allowed to manage CloudFormation, ECS, ELB, security
   groups, logs, and IAM roles, including passing the execution role to ECS.
   Image and secret preparation also requires ECR and Secrets Manager access.

Fargate tasks have private addresses and no public IP assignment. See
[AWS's Fargate networking guide](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-task-networking.html)
for subnet routing requirements. Endpoint security groups must admit HTTPS from
the Forge task security group. Network ACLs must allow return traffic.

## Build and publish an image

These commands are deployment instructions; run them only when preparing an AWS
rollout. Set your actual region and account ID:

```sh
export AWS_REGION=us-east-1
export AWS_ACCOUNT_ID=123456789012
export AWS_PAGER=''
export FORGE_REGISTRY="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com"
export FORGE_RELEASE="release-$(git rev-parse --short HEAD)"

mkdir -p .tools/aws-image
curl --fail --location --proto '=https' --tlsv1.2 \
  https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
  -o .tools/aws-image/rds-global-bundle.pem

docker build --platform linux/amd64 -t forge:release .
docker build --platform linux/amd64 -f deploy/aws/Dockerfile \
  -t "$FORGE_REGISTRY/forge:$FORGE_RELEASE" .tools/aws-image
```

The cloud image adds the RDS trust bundle to the base image while retaining its
non-root user. Database connections below use certificate and hostname
verification. Review the bundle when preparing releases and refresh it for CA
rotation. AWS publishes the [RDS certificate bundles](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.SSL.html).

Create the repository once, or use an existing `forge` repository:

```sh
aws ecr create-repository --repository-name forge \
  --image-tag-mutability IMMUTABLE \
  --image-scanning-configuration scanOnPush=true
```

Publish and obtain the immutable image reference:

```sh
aws ecr get-login-password --region "$AWS_REGION" |
  docker login --username AWS --password-stdin "$FORGE_REGISTRY"
docker push "$FORGE_REGISTRY/forge:$FORGE_RELEASE"
FORGE_DIGEST=$(aws ecr describe-images --repository-name forge \
  --image-ids "imageTag=$FORGE_RELEASE" --query 'imageDetails[0].imageDigest' --output text)
printf '%s/forge@%s\n' "$FORGE_REGISTRY" "$FORGE_DIGEST"
```

Copy that digest reference into `ImageUri` in the parameter file below. Review
image scan findings before release. The template requires a digest rather than
a mutable tag and selects Linux x86_64 tasks to match this build.

## Store credentials

Create two Secrets Manager secrets using **plain string** values, not JSON
objects. Use the AWS-managed Secrets Manager encryption key for this template.
If using a customer-managed key, add appropriately scoped `kms:Decrypt`
permission to the execution role and authorize it in the key policy.

`DatabaseSecretArn` must reference a connection URL of this form, with the actual
RDS hostname and URL-encoded username/password:

```text
postgres://forge:PASSWORD@YOUR_RDS_HOST:5432/forge?sslmode=verify-full&sslrootcert=/etc/ssl/certs/rds-global-bundle.pem&pool_max_conns=8&pool_min_conns=1
```

`ApiTokenSecretArn` must reference a randomly generated token of at least 32
bytes without whitespace. For example, prepare ignored credential files with
private permissions, enter the database URL in `.secrets/cloud_database_url`,
then upload them without putting their contents on the command line:

```sh
umask 077
mkdir -p .secrets
python3 -c 'import secrets; from pathlib import Path; Path(".secrets/cloud_api_token").write_text(secrets.token_hex(32))'
# Create .secrets/cloud_database_url in your editor before continuing.
aws secretsmanager create-secret --name forge/database-url \
  --secret-string file://.secrets/cloud_database_url --query ARN --output text
aws secretsmanager create-secret --name forge/api-token \
  --secret-string file://.secrets/cloud_api_token --query ARN --output text
```

Do not include a trailing newline in the database URL. Retain the returned ARNs
for the parameters. ECS retrieves secrets using the execution role and injects
them at task startup. Secret changes require replacement tasks; see
[ECS secret injection](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/secrets-envvar-secrets-manager.html).
The application does not receive an AWS task role or query AWS APIs itself.

## Provision with services disabled

```sh
mkdir -p .tools
cp deploy/aws/parameters.example.json .tools/cloud-parameters.json
```

Replace every placeholder in that file with actual identifiers. Keep
`EnableServices=false`. Set `AllowedIngressCidr` to the IPv4 range that should
reach the HTTPS endpoint. All supplied subnets and security groups must belong
to the supplied VPC. The certificate must cover the hostname you will configure.

```sh
aws cloudformation validate-template --template-body file://deploy/aws/stack.yaml
aws cloudformation deploy --stack-name forge \
  --template-file deploy/aws/stack.yaml \
  --parameter-overrides file://.tools/cloud-parameters.json \
  --capabilities CAPABILITY_IAM
aws cloudformation describe-stacks --stack-name forge \
  --query 'Stacks[0].Outputs' --output table
```

The stack creates the ALB, cluster, task definitions, service definitions, role,
logs, and security-group rules. Both services have zero desired tasks until
explicitly enabled. The ALB and other prepared infrastructure can incur charges
even while services are disabled. See the
[CloudFormation deploy command](https://docs.aws.amazon.com/cli/latest/reference/cloudformation/deploy.html)
for change-set review and update options.

## Apply migrations

Fetch the migration definition and private network configuration from the stack:

```sh
aws cloudformation describe-stacks --stack-name forge --output json > .tools/cloud-stack.json
FORGE_CLUSTER=$(jq -r '.Stacks[0].Outputs[] | select(.OutputKey=="ClusterName") | .OutputValue' .tools/cloud-stack.json)
FORGE_MIGRATION=$(jq -r '.Stacks[0].Outputs[] | select(.OutputKey=="MigrationTaskDefinition") | .OutputValue' .tools/cloud-stack.json)
jq '{awsvpcConfiguration: {
  subnets: (.Stacks[0].Parameters[] | select(.ParameterKey=="PrivateSubnetIds") | .ParameterValue | split(",")),
  securityGroups: [.Stacks[0].Outputs[] | select(.OutputKey=="TaskSecurityGroupId") | .OutputValue],
  assignPublicIp: "DISABLED"
}}' .tools/cloud-stack.json > .tools/cloud-network.json

aws ecs run-task --cluster "$FORGE_CLUSTER" --launch-type FARGATE \
  --platform-version 1.4.0 --task-definition "$FORGE_MIGRATION" \
  --network-configuration file://.tools/cloud-network.json \
  --count 1 --output json > .tools/cloud-migration.json
jq -e '(.failures | length) == 0 and (.tasks | length) == 1' .tools/cloud-migration.json
```

Continue only if the launch check above succeeds. Wait for the task and inspect
its exit status:

```sh
FORGE_MIGRATION_TASK=$(jq -r '.tasks[0].taskArn' .tools/cloud-migration.json)
aws ecs wait tasks-stopped --cluster "$FORGE_CLUSTER" --tasks "$FORGE_MIGRATION_TASK"
aws ecs describe-tasks --cluster "$FORGE_CLUSTER" --tasks "$FORGE_MIGRATION_TASK" \
  --output json > .tools/cloud-migration-result.json
jq -e '(.failures | length) == 0 and (.tasks | length) == 1 and
  (.tasks[0].containers | length) == 1 and
  .tasks[0].containers[0].exitCode == 0' .tools/cloud-migration-result.json
```

Proceed only when the exit-code check succeeds. A stopped task alone does not
prove migration success. On failure, inspect the stopped reason and the
`migrate/forge/…` stream in the stack's `LogGroupName` output. Fix database
connectivity, TLS, credentials, or schema permissions before launching another
migration task. Migrations use a database advisory lock and version tracking.
CloudFormation does not run or verify this task automatically.

## Enable the endpoint

Change `EnableServices` to `true` in `.tools/cloud-parameters.json`, then repeat
the deploy command. This starts two API replicas and ten worker replicas with
the defaults. ECS deployment circuit breakers roll back failed service rollouts;
they are separate from Forge's token and runtime circuit breakers.

Create a Route 53 alias, or a subdomain CNAME with your DNS provider, pointing the
certificate's hostname to `LoadBalancerDNS`. Route 53 aliases use the stack's
`LoadBalancerHostedZoneId` output. The raw ALB hostname will not match your ACM
certificate; use the configured hostname for HTTPS access.

Check readiness and open the console at your chosen hostname:

```sh
curl --fail https://YOUR_FORGE_HOST/readyz
```

Enter the API token in **Connection settings**. Confirm that the worker view
shows ten active workers, submit a small workflow, and verify its results before
running the [HTTP load test](load-testing.md). The shell and readiness endpoints
are unauthenticated; workflow operations and metrics require the bearer token.
There is no multi-user identity or tenant isolation in this reference deployment.

## Operations

The default application pool limit yields up to 96 database connections across
two API tasks and ten workers. Rolling replacement can temporarily double that
to 192, plus migration and administrative connections. Budget database capacity,
Fargate quotas, subnet addresses, and CPU/memory before increasing replicas.
Task slots are cooperative simulator concurrency, not a CPU allocation.

Container logs go to CloudWatch with 30-day retention. The log group is retained
when the stack is removed. Worker metrics stay private on port 9091; API metrics
are available on port 8080 and through the authenticated HTTPS endpoint. To
scrape private targets, supply `MonitoringSecurityGroupId` and configure your
Prometheus deployment to discover each task's ENI address, refreshing targets as
tasks are replaced. This template does not provision a monitoring system or
implement ECS discovery. See [observability](observability.md) for metrics and
authentication. The Compose DNS configuration is specific to Compose networks.

Trace export is disabled in this cloud template. To enable it, add
`OTEL_EXPORTER_OTLP_ENDPOINT` to the API and worker task environments, pointing
to your reachable OTLP/HTTP collector. Current task egress allows HTTPS on 443;
adjust network rules if your collector uses a different port. Configure the
collector's backend exporter separately. This does not happen merely by setting
a region label in Forge.

For a schema-changing release, use a maintenance window: disable services,
deploy the new image digest with services still disabled, run and verify the
new migration task, then enable services. Existing pending work remains in
PostgreSQL; interrupted tasks recover under lease and attempt limits. Do not
roll back to an older binary unless it supports the current schema. For a
compatible code-only update, change the digest and redeploy with services
enabled. No automatic schema rollback is provided.

For credential rotation, update the database and secret coherently, then force
new deployments of both ECS services. CloudFormation does not detect changed
secret contents. Restrict access to deployment and debugging tools that can
read container environments.

To retire a deployment, disable services and verify tasks have stopped before
removing the stack. The external database, secrets, ECR repository, DNS records,
network infrastructure, certificate, and retained log group have independent
lifecycles. Removing this execution-plane stack does not remove those resources.

## Validation scope

The template's YAML, intrinsic references, dependency graph, parameter examples,
and application configuration have been checked offline. Image builds, AWS
template acceptance, IAM permissions, TLS routing, migration execution, and
runtime health still require a deployment test. No AWS resources were provisioned
as part of preparing these files.
