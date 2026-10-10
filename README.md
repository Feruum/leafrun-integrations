# leafrun-integrations

Planned Git and cloud integrations: OAuth connections, synchronization and conflict handling.

**Status: planned component.** This repository currently contains its scope and roadmap links. It does not yet provide a runnable service or package. The existing local rendering prototype lives in [leafrun-generation](https://github.com/leafrun/leafrun-generation).

## Responsibility

Connections, OAuth provider state, sync cursors, conflicts, external data snapshots.

Owns the logical PostgreSQL database `integrations_db`, its credentials and migrations. Other domains access this data through contracts, never cross-domain SQL.

Planned process roles:

- `api-webhook-ingress`
- `sync-workflow-workers`
- `connector-activity-workers`

## Dependencies

- [leafrun-contracts](https://github.com/leafrun/leafrun-contracts)
- [leafrun-platform](https://github.com/leafrun/leafrun-platform)
- `sync-core`
- `collaboration-api`
- `storage-api`
- `secret-store`

## Boundaries

No unique durable local state; activity scratch and workflow replay cache disposable. API and worker processes scale and release independently. S3 holds bytes; domain databases hold durable metadata. Redis is a disposable cache; NATS JetStream carries domain events; Temporal owns workflow task queues.

## Roadmap

- [LEA-12 — Project sources and immutable snapshots](https://linear.app/bulnik/issue/LEA-12/e02-istochniki-proekta-i-neizmenyaemye-snimki)
- [LEA-73 — Two-way synchronization](https://linear.app/bulnik/issue/LEA-73/e10-dvustoronnyaya-sinhronizaciya)
- [LEA-82 — Live collaboration and project ownership](https://linear.app/bulnik/issue/LEA-82/e11-live-collaboration-i-vladelec-proekta)
- [LEA-122 — Future capabilities: discovery](https://linear.app/bulnik/issue/LEA-122/e16-dalnejshie-vozmozhnosti-issledovanie)

See the [development roadmap](https://github.com/leafrun/leafrun/blob/main/docs/roadmap.md) for dependencies and acceptance milestones. Creating this repository does not complete its implementation issues.

## Contributing

Use English for code comments, documentation, examples, commit messages and pull requests. See the [contribution guide](https://github.com/leafrun/leafrun/blob/main/CONTRIBUTING.md) and [current architecture](https://github.com/leafrun/leafrun/blob/main/docs/architecture.md).

## License status

A project license has not been selected. No license is assigned by this repository scaffold.
