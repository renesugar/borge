# evidence/tsa

Root certificates pinned for offline verification of the RFC 3161 timestamp tokens on
borge's evidence artifacts. A token carries its own intermediate chain; the root is the
part a verifier must already trust, and trusting the machine's system store makes the
verification depend on whatever that store happens to hold years from now.

| file | root | SHA-256 of this file |
| --- | --- | --- |
| `digicert-trusted-root-g4.pem` | `C=US, O=DigiCert Inc, OU=www.digicert.com, CN=DigiCert Trusted Root G4` | `ce7d6b44f5d510391be98c8d76b18709400a30cd87659bfebe1c6f97ff5181ee` |
| `usertrust-rsa-certification-authority.pem` | `C=US, ST=New Jersey, L=Jersey City, O=The USERTRUST Network, CN=USERTrust RSA Certification Authority` | `8a3dbcb92ab1c6277647fe2ab8536b5c982abbfdb1f1df5728e01b906aba953a` |

Certificate fingerprints (SHA-256 of the DER certificate, not of the file):

- DigiCert Trusted Root G4 — `55:2F:7B:DC:F1:A7:AF:9E:6C:E6:72:01:7F:4F:12:AB:F7:72:40:C7:8E:76:1A:C2:03:D1:D9:D2:0A:C8:99:88`, valid 2013-08-01 to 2038-01-15.
- USERTrust RSA Certification Authority — `E7:93:C9:B0:2F:D8:AA:13:E2:1C:31:22:8A:CC:B0:81:19:64:3B:74:9C:89:89:64:B1:74:6D:46:C3:D4:CB:D2`, valid 2010-02-01 to 2038-01-18.

**Provenance.** Both were copied on 2026-08-27 from this machine's `ca-certificates`
package (Ubuntu `20260601~24.04.1`), from `/etc/ssl/certs/DigiCert_Trusted_Root_G4.pem`
and `/etc/ssl/certs/USERTrust_RSA_Certification_Authority.pem`. They are the roots the
DigiCert and Sectigo timestamp chains actually terminate in, read out of live tokens
rather than assumed: the Sectigo signer chains through `Sectigo Public Time Stamping CA
R41` and `Sectigo Public Time Stamping Root R46` to USERTrust, and the DigiCert signer
through `DigiCert Trusted G4 TimeStamping RSA4096 SHA256 2025 CA1` to Trusted Root G4.

**Which root goes with which token is checked.** A DigiCert token does not verify against
the USERTrust root and a Sectigo token does not verify against the DigiCert root; the
catalog names the file for each token and `scripts/verify-evidence.py` uses that one alone,
so a token that changed authority cannot pass on the other's root.

A pinned root eventually expires (both in 2038) and an authority can rotate before then.
When that happens, add the new root as another file and leave this one in place: old
tokens must stay verifiable.
