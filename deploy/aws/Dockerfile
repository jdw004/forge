# Build the base image as forge:release, then use a context containing only the
# downloaded RDS certificate bundle. See docs/cloud-deployment.md.
FROM forge:release
COPY --chmod=0444 rds-global-bundle.pem /etc/ssl/certs/rds-global-bundle.pem
