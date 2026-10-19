# India DLT at launch

India launches with full DLT support. Before a message reaches an operator, the platform checks its entity, header, template, category and telemarketer chain. A message that would fail operator scrubbing is rejected by us first, with a clear reason.

| DLT item | What the platform does | Schema (`migrations/postgres/0003_dlt.sql`) |
| --- | --- | --- |
| Principal Entity (PE) ID | Stored per customer/sub-account, attached to every message | `dlt_entities` |
| Headers (sender IDs) | Registered per PE with category; unregistered header → reject | `dlt_headers` |
| Content templates | Template ID + body with `{#var#}`; each message matched against the header's templates; template ID found automatically when omitted | `dlt_templates`, `dlt_template_headers` |
| Categories | Transactional, service implicit, service explicit, promotional; promotional hours 09:00–21:00 IST; consent for service explicit | `category` columns, CHECK on consent |
| Consent templates | Linked to service-explicit templates | `dlt_consent_templates` |
| Telemarketer chain | Our TM ID and the PE–TM chain value sent with every message | `telemarketers`, `dlt_tm_chains` |
| URL, APK, callback numbers | Must be whitelisted on the template | `dlt_template_whitelist` |
| Bulk upload and sync | CSV/Excel import; operator portal sync where an API exists | phase 3 |
| DLT fields on SMPP/HTTP | PE ID, template ID, TM chain as HTTP fields and SMPP TLVs; TLV tags configurable per vendor | `vendor_binds.tlv_profile`, `api/openapi.yaml` |
| Scrubbing failures | Operator DLT rejection codes mapped to customer error codes and DLR statuses | phase 3 |
| Reports | Traffic and failures by PE, header, template, category | ClickHouse `message_records` |

Before building the connectors, confirm with each Indian vendor the exact TLV tags and the telemarketer chain format they expect.
