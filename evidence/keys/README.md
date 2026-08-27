# evidence/keys

`evidence-signing-public.asc` is the OpenPGP public key that verifies the detached
signatures on borge's evidence artifacts. It is checked in so that verification needs
neither a keyserver nor a network connection.

| field | value |
| --- | --- |
| identity | `Rene Sugar (Evidence Identity) <rene.sugar@gmail.com>` |
| primary key | `AEE5F82F2C216D6D15992C8DC96A1C6039BC8098`, Ed25519, created 2026-08-25, **certify-only** |
| signing subkey | `4ABEB98AF99C8321931BCF282C6A8A4568264005`, Ed25519, created 2026-08-25, expires 2027-08-25 |
| file SHA-256 | `7eb4dd124580572f202c8368ccc3685b9978abf4c6ff0d945cd073ae0c466952` |

The primary key certifies; it does not sign artifacts. Its secret half is deliberately
absent from this machine — `gpg -K` prints `sec#`, and the `#` is the whole point: a
signing key that can be replaced without replacing the identity is worth more than one
kept where the signing happens.

Import and check:

```bash
gpg --import evidence/keys/evidence-signing-public.asc
gpg --verify /home/renes/evidence/borge/<artifact>.asc /home/renes/evidence/borge/<artifact>
```

`scripts/verify-evidence.py` does this itself, against a temporary keyring built from this
file alone, so a signature from some other key in the operator's keyring cannot pass.

## What is not here, and must not be

The primary secret key, its backup, and the revocation certificate belong on offline
media, not in a public repository. Publishing a revocation certificate hands anyone the
ability to revoke the identity; publishing the primary secret key ends it. Record where
those live in the custody log kept with the ISO masters.

When the subkey expires or is rotated, add the new public key beside this one rather than
replacing it: signatures made by the old subkey must stay verifiable after it stops being
used.
