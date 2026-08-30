#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prepare the local services the remote-backend tests use (plans/PORTING_PLAN.md §11.5).
#
#   tests/remote/setup.sh
#
# Idempotent: run it as often as you like. It configures nothing that is not scoped to
# localhost, and the teardown for every part is listed at the bottom of this file.
#
# What it needs already running:
#   sftpgo      systemctl start sftpgo      (SFTP on :2022, admin API on :8080)
#   LocalStack  lstk start                  (S3 on :4566)
#   rclone      on PATH                     (no service; a "local" remote needs no config)
#
# What it needs from the secret store:
#   secret-tool lookup application sftpgo user admin    - the sftpgo admin password
#
# It prints the repository URLs the tests use. Nothing here writes into the borge tree.

set -euo pipefail

SFTP_ALIAS=borge-sftp-test
SFTP_USER=borge
SFTP_PORT=2022
SFTP_HOME=/var/lib/sftpgo/borge-test
SFTP_KEY=~/.ssh/borge_sftp_test
S3_BUCKET=borge-test-1
S3_ENDPOINT=http://localhost:4566

note() { printf '%s\n' "$*" >&2; }

# ---------------------------------------------------------------- sftpgo
#
# borg's sftp backend does NOT do password authentication: borgstore's URL has no password
# field and its paramiko connect() is called with key_filename and allow_agent only. So the
# test user authenticates with a key, and the connection details live in ~/.ssh/config so
# that no secret ever appears in a repository URL.
setup_sftp() {
    if ! curl -sf -o /dev/null "http://127.0.0.1:8080/healthz"; then
        note "sftpgo: not reachable on :8080 - skipping (systemctl start sftpgo)"
        return
    fi
    local admin_pw token pub body code
    admin_pw=$(secret-tool lookup application sftpgo user admin 2>/dev/null || true)
    if [ -z "$admin_pw" ]; then
        note "sftpgo: no admin password in secret-tool (application=sftpgo user=admin) - skipping"
        return
    fi

    if [ ! -f "${SFTP_KEY/#\~/$HOME}" ]; then
        ssh-keygen -q -t ed25519 -N "" -C "borge sftp test (localhost:$SFTP_PORT)" \
            -f "${SFTP_KEY/#\~/$HOME}"
        note "sftpgo: generated $SFTP_KEY"
    fi
    pub=$(cat "${SFTP_KEY/#\~/$HOME}.pub")

    token=$(curl -s -u "admin:$admin_pw" "http://127.0.0.1:8080/api/v2/token" |
        python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
    body=$(python3 -c '
import json, sys
print(json.dumps({
    "username": sys.argv[1], "home_dir": sys.argv[2], "status": 1,
    "permissions": {"/": ["*"]}, "public_keys": [sys.argv[3]],
}))' "$SFTP_USER" "$SFTP_HOME" "$pub")

    # PUT updates, POST creates: try the update first so a re-run does not fail.
    code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
        -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
        -d "$body" "http://127.0.0.1:8080/api/v2/users/$SFTP_USER")
    if [ "$code" = "404" ]; then
        code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
            -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
            -d "$body" "http://127.0.0.1:8080/api/v2/users")
    fi
    note "sftpgo: user '$SFTP_USER' -> HTTP $code"

    # borgstore enforces known_hosts (it sets no missing-host-key policy on purpose), so the
    # host key has to be there before the first connection rather than accepted at it.
    if ! ssh-keygen -F "$SFTP_ALIAS" >/dev/null 2>&1; then
        ssh-keyscan -p "$SFTP_PORT" -t ed25519 127.0.0.1 2>/dev/null |
            sed "s|^\[127.0.0.1\]:$SFTP_PORT|$SFTP_ALIAS|" >> ~/.ssh/known_hosts
        note "sftpgo: added the host key for $SFTP_ALIAS to ~/.ssh/known_hosts"
    fi

    if ! grep -q "^Host $SFTP_ALIAS\$" ~/.ssh/config 2>/dev/null; then
        touch ~/.ssh/config && chmod 600 ~/.ssh/config
        cat >> ~/.ssh/config <<EOC

# Added by borge's tests/remote/setup.sh (PORTING_PLAN §11.5).
Host $SFTP_ALIAS
    HostName 127.0.0.1
    Port $SFTP_PORT
    User $SFTP_USER
    IdentityFile $SFTP_KEY
    IdentitiesOnly yes
EOC
        note "sftpgo: added 'Host $SFTP_ALIAS' to ~/.ssh/config"
    fi
}

# ---------------------------------------------------------------- LocalStack
setup_s3() {
    if ! curl -sf -o /dev/null "$S3_ENDPOINT/_localstack/health"; then
        note "localstack: not reachable on :4566 - skipping (lstk start)"
        return
    fi
    if ! AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1 \
        aws --endpoint-url "$S3_ENDPOINT" s3 ls "s3://$S3_BUCKET" >/dev/null 2>&1; then
        AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1 \
            aws --endpoint-url "$S3_ENDPOINT" s3 mb "s3://$S3_BUCKET" >/dev/null
        note "localstack: created bucket $S3_BUCKET"
    else
        note "localstack: bucket $S3_BUCKET is there"
    fi
}

setup_sftp
setup_s3

cat <<EOU

Repository URLs for the tests (all verified against the reference borg):

  sftp    sftp://$SFTP_ALIAS/REPO
  s3      s3:test:test@$S3_ENDPOINT/$S3_BUCKET/REPO
  rclone  rclone:/ABSOLUTE/PATH/REPO
  rest    rest:///ABSOLUTE/PATH/REPO      (local: borge serve --rest over stdio)

To undo everything this script did:

  ssh config   remove the "Host $SFTP_ALIAS" block from ~/.ssh/config
  ssh key      rm $SFTP_KEY $SFTP_KEY.pub
  known_hosts  ssh-keygen -R $SFTP_ALIAS
  sftpgo user  curl -X DELETE -H "Authorization: Bearer \$TOKEN" \\
                 http://127.0.0.1:8080/api/v2/users/$SFTP_USER
  s3 bucket    awslocal s3 rb --force s3://$S3_BUCKET
EOU
