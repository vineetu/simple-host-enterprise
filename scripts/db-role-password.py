#!/usr/bin/env python3
"""Print an ALTER ROLE statement that sets a Postgres role's password from a
secrets.env value, as a SCRAM-SHA-256 verifier: the password itself is never
printed, put on a command line or sent to the server. This is `\\password`
for an agent or script with no terminal. Pipe it straight into psql:

  python3 scripts/db-role-password.py deploy/overlays/byo/secrets.env DB_PASSWORD simplehost | psql "<admin connection>"

The verifier is a salted hash, not the password, but it can still be
attacked offline: do not keep the output.
"""
import base64
import hashlib
import hmac
import os
import re
import sys

ITERATIONS = 4096  # Postgres's default scram_iterations


def verifier(password: str) -> str:
    salt = os.urandom(16)
    salted = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, ITERATIONS)
    client_key = hmac.new(salted, b"Client Key", hashlib.sha256).digest()
    stored_key = hashlib.sha256(client_key).digest()
    server_key = hmac.new(salted, b"Server Key", hashlib.sha256).digest()
    b64 = lambda b: base64.b64encode(b).decode()
    return f"SCRAM-SHA-256${ITERATIONS}:{b64(salt)}${b64(stored_key)}:{b64(server_key)}"


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    path, key, role = sys.argv[1:]
    if not re.fullmatch(r"[a-z_][a-z0-9_]*", role):
        print(f"db-role-password: role name {role!r} must be a plain lower-case identifier", file=sys.stderr)
        return 2
    value = None
    with open(path, encoding="utf-8") as f:
        for line in f:
            name, sep, rest = line.rstrip("\r\n").partition("=")
            if sep and name.strip() == key:
                value = rest.strip()
    if not value:
        print(f"db-role-password: {key} is missing or empty in {path}", file=sys.stderr)
        return 1
    if not value.isascii():
        # Postgres applies SASLprep to non-ASCII passwords; the generated ones are hex.
        print(f"db-role-password: {key} is not ASCII; set it with \\password instead", file=sys.stderr)
        return 1
    print(f"ALTER ROLE {role} PASSWORD '{verifier(value)}';")
    return 0


if __name__ == "__main__":
    sys.exit(main())
