import importlib.util
import os
from pathlib import Path
import stat
import tempfile
import unittest
from urllib.parse import parse_qs, urlparse

spec = importlib.util.spec_from_file_location("configure_compose", Path(__file__).with_name("configure-compose.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ConfigureTests(unittest.TestCase):
    def test_consistent_credentials_and_bounded_pool(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "secrets"
            module.configure(destination)
            password = (destination / "postgres_password").read_text().strip()
            token = (destination / "api_token").read_text().strip()
            url = urlparse((destination / "database_url").read_text().strip())
            self.assertEqual(len(token), 64)
            self.assertEqual(len(password), 48)
            self.assertNotEqual(password, token)
            self.assertEqual(url.password, password)
            self.assertEqual(url.hostname, "postgres")
            self.assertEqual(parse_qs(url.query)["pool_max_conns"], ["8"])
            if os.name == "posix":
                self.assertEqual(stat.S_IMODE(destination.stat().st_mode), 0o700)
                self.assertEqual(stat.S_IMODE((destination / "api_token").stat().st_mode), 0o444)

    def test_existing_secrets_are_never_overwritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "secrets"
            module.configure(destination)
            before = {path.name: path.read_bytes() for path in destination.iterdir()}
            with self.assertRaises(FileExistsError):
                module.configure(destination)
            self.assertEqual(before, {path.name: path.read_bytes() for path in destination.iterdir()})


if __name__ == "__main__":
    unittest.main()
