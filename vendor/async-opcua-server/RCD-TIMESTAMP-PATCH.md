# Opt-in source timestamp preservation (2026-10-15)

The `preserve-source-timestamps` feature is enabled only by the independent
`communicate` crate. It is not a default feature. Existing RCD builds do not
enable it and retain their previous notification scheduling behavior.

Upstream 0.19.0 schedules some delayed notifications by moving their source
timestamps forward. This breaks gateways which retain the last successful
sample timestamp when only communication quality changes. With this feature,
monitored items track notification sampling time separately, coalesce to the
latest value, and never rewrite the supplied source timestamp. Publication uses
the existing subscription timer and bounded queues; no additional worker exists.

Regression coverage lives in `subscriptions/monitored_item.rs`. Run the server
library tests both without and with this feature using the fork's manifest in
the isolated directory on 46; also run the communicate two-security-mode OPC
interoperability checks. Commands and the missing upstream sample-file test
exclusion are recorded in `communicate/docs/validation-20261015.md`. This patch
does not claim physical device acceptance or alter the old
RCD executable. The existing StatusValue notification patch remains in place.
