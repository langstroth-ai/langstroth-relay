# Security policy

The relay forwards end-to-end-encrypted envelopes and must never be able to
read plaintext. Reports about access-token gating, the `hrt1` token format,
tenancy isolation, the phone tunnel, refusal limits or anything that lets the
relay (or someone who controls it) learn content are in scope.

**Please don't open a public issue for a vulnerability.** Report it privately
using the contact in the Ligus
[security policy](https://github.com/langstroth-ai/ligus/blob/main/SECURITY.md),
naming this repository, the version or commit, reproduction steps and the
impact.

For what each component can see, read the
[threat model](https://docs.langstroth.ai/operate/security/threat-model/) and
the [security overview](https://docs.langstroth.ai/operate/security/).
