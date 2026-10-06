# Security policy

tokenpool holds API keys, so security reports matter here.

## Reporting a vulnerability

Please don't open a public issue. Report it privately through GitHub:
open the repository's **Security** tab and choose **Report a
vulnerability**. Include what you found, how to reproduce it, and the
version or commit you tested.

You should get a reply within a week. Fixes land on `main`, and the
advisory is published once a fix is available.

## Scope

In scope: anything that lets a caller without a valid key use the pool,
reach the admin API, read upstream keys, or make tokenpool send a key to a
host other than the upstream it belongs to.

Out of scope: running with `allow_anonymous: true`, exposing tokenpool's
plain-HTTP port to an untrusted network, and a config file that other
users can read (tokenpool warns about that one at startup).
