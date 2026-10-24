# Local patches

This directory vendors `async-opcua-server` 0.19.0 from crates.io under its
MPL-2.0 license.

The local patch changes the default monitored-item data-change comparison to
compare both `DataValue.value` and `DataValue.status`. OPC UA's default trigger
is `StatusValue`; without this change, a transition such as
`0 / Good -> 0 / BadNoCommunication` is not reported to subscribers.
