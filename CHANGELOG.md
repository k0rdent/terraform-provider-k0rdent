# Changelog

All notable changes to this provider are documented in this file, and
provider versions use Semantic Versioning.

Each release's section is rendered by `make changelog` from the entries in
[`.changelog/`](.changelog/README.md), in
[HashiCorp's changelog format](https://developer.hashicorp.com/terraform/plugin/best-practices/versioning#changelog-specification):
BREAKING CHANGES, NOTES, FEATURES, ENHANCEMENTS and BUG FIXES, with each line
naming the resource it changes. A pull request adds its entry there, not here.

Changes merged before `.changelog/` existed are in the `[Unreleased]` section
below, written by hand in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. They stay as
written and ship with the first release, under a heading of their own that
[`RELEASE.md`](RELEASE.md) describes.

## [Unreleased]

### Added

- A list resource for every managed resource, for `terraform query` on
  Terraform 1.14 and later. Each finds what a tenant can adopt, by the
  resource's identity, through the same paged list calls the plural data
  sources use. `terraform query -generate-config-out` writes an `import`
  block and a `resource` block for each object found, and the object in
  each, which `include_resource = true` also shows, is what an import of
  that identity reads. A project-scoped list searches the provider's project
  unless its `config` names another. Networks, subnets and security groups
  the platform made for itself are left out unless
  `include_platform_owned = true`. Objects being deleted and expired
  credentials are left out. An object whose read fails is named in a warning
  and the rest still list, and a list cut at its `limit` says so. A webhook
  subscription is shown by its id rather than its URL.
  The subnet and peering lists search every network or VPC unless `config`
  names one, and the service account and credential lists take
  `organization_id` and, optionally, a `scope`. The import guide's bulk
  adoption now starts there. Older Terraform never asks for a list resource,
  so nothing changes for it.
- `default_tags` on the provider block, the tags every resource that carries
  `tags` gets: `k0rdent_kubernetes_cluster`, `k0rdent_network`,
  `k0rdent_security_group` and `k0rdent_vpc_peering`. Each gains a computed
  `tags_all`, the defaults with its own `tags` on top, known at plan whenever
  both are. Changing or removing a default updates each resource in place.
  A `default_tags` value known only at apply leaves `tags_all` unknown, so
  each of those but the peering plans an in-place update, and one whose tags
  come out the same sends nothing. A VPC peering's tags are set only at
  creation, so it keeps the ones it was created with and the plan warns, and
  a change to its `tags` that leaves `tags_all` the same, such as moving a
  tag into `default_tags` with the same value, no longer replaces it. A resource tag equal to a
  default plans nothing on the next run. A state from an earlier version
  starts `tags_all` as its `tags`. The provider's create-token key is refused
  in `tags` and `default_tags`, since it never reads back.
- Filters on the plural data sources, so a module can find "the network
  tagged `env = "prod"`" without hardcoding its id. `tags` (every pair has to
  match) on `k0rdent_networks`, `k0rdent_subnets`, `k0rdent_security_groups`
  and `k0rdent_vpc_peerings`. `display_name_regex` on every plural whose
  objects have a display name, in RE2 syntax and checked at plan. `state` or
  `status`, under the name the objects use, on every plural whose objects
  carry one and didn't take it, and `is_active` on
  `k0rdent_webhook_subscriptions`. Where the list operation takes the filter
  (`state` on security groups, `inStates` on instances, `status` on
  projects) it's sent to the platform. The rest run in the provider after
  the whole list is read, and each attribute's description says which.
  `status = "deprecated"` on `k0rdent_cluster_types` asks the platform for
  deprecated types too, and beside `include_deprecated = false` it's refused
  at plan. `state = "deleted"` on `k0rdent_instances` returns the records of
  released hosts, which the platform keeps. A `tags` filter value of null is
  refused at plan. The network, subnet, security group and peering lists now
  report each object's `tags`, null when it has none as on the resources,
  and the subnet and peering lists its `display_name`, so what a filter
  matched can be read back.
- `region` on `k0rdent_project`, the region the API locks a
  project to. It defaults to the provider's `region`, is sent on
  the create the platform now refuses without it, and is read back. A region
  that holds no projects is refused at plan, naming the ones that do. The
  `k0rdent_project` and `k0rdent_projects` data sources report it.
- `hosts_projects` on each region of the `k0rdent_regions` data source, and
  `project_region_ids`, the regions a project can be created in. The contract
  asks a client offering a project region to offer these.
- `storage_ceiling_gib` on `k0rdent_kubernetes_cluster`, the storage the
  cluster declares against the `storage_gib` quota. It's raised in place, and
  a lower value is refused at plan, since the platform never lowers one. A
  raise refused while the cluster or its quota accounting is settling is sent
  again once the cluster settles.
- A regional resource created in a region its project isn't in is refused at
  plan, naming the project's region, when the project list shows it. Any
  request the platform refuses with `409 PROJECT_REGION_MISMATCH` names the
  project's region too, which the platform's answer doesn't.
- `on_destroy_busy` on `k0rdent_kubernetes_cluster`: what a destroy does when
  the cluster is `creating` or `updating`, which the platform refuses to
  delete. `cancel`, the default, cancels the running operation first, as every
  destroy did before. `wait` waits for the operation to finish and then
  deletes the cluster, within the delete timeout. It's never sent to the
  platform, and a create whose wait times out records it, so the destroy of
  that tainted cluster follows it. Changing it sends nothing and waits for
  nothing, even while the cluster is mid-operation.

- `k0rdent_webhook_subscription`, over `createWebhookSubscription`,
  `listWebhookSubscriptions`, `deleteWebhookSubscription` and
  `rotateWebhookSubscriptionSecret`, and the `k0rdent_webhook_subscription` and
  `k0rdent_webhook_subscriptions` data sources, which add each endpoint's
  delivery health. The signing secret is `secret_wo`, a write-only attribute:
  the platform answers a secret once, and this provider keeps no secret in
  state or a plan, so the configuration holds it and Terraform never stores
  it. Creating or replacing a subscription needs it, and write-only
  attributes need Terraform 1.11, so making one does too. The provider's
  floor stays 1.10, and an existing subscription can be imported and managed
  there without a secret. Changing `secret_wo_version` rotates
  the same subscription in place rather than replacing it, which keeps its id
  and everything queued for it, with the platform's 24-hour overlap unless
  `rotation_overlap = false`. `url` and `event_types` replace, because there's
  no update. A rotation made outside Terraform is noticed on refresh, and the
  next plan rotates back. An event type outside the catalog's form is warned
  about, not refused. The schema credential gate now lets a write-only
  attribute carry a credential, because Terraform writes it to neither state
  nor a plan.
- A diagnostic for a successful answer the provider couldn't read names the
  status and content type, and no longer quotes the body, which can carry a
  value the platform shows only once.
- `k0rdent_service_account`, over `createServiceAccount`, `getServiceAccount`,
  `updateServiceAccount` and `deleteServiceAccount`, with a
  `k0rdent_service_account` data source. It's the identity a pipeline
  authenticates as, with its `roles` bound at one `scope` and `scope_id`.
  A role is named by its id, such as `compute-admin`, which the new
  `k0rdent_roles` data source lists over `listRoles`. A dotted name, as in the
  contract's examples, isn't a role id.
  Display name, description, roles and `status` change in place, and
  `status = "disabled"` revokes the account's live tokens. A change to the
  organization, scope or id replaces it, which ends every pipeline using it,
  so the plan refuses unless `allow_replace = true` is set.
- `k0rdent_service_account_credential`, over the credential operations. The
  platform answers a credential's secret once, at create, and no generated secret may reach state in
  readable form, so the secret is encrypted
  to the OpenPGP public key in `pgp_key` and state holds only the ciphertext,
  `sealed_client_secret`. Open it with `gpg --decrypt`. This is the one
  attribute the schema credential gate excepts, and only while it's computed,
  unset by configuration and not marked Sensitive. Every argument replaces the
  credential, which is how it rotates: change `expires_at`, with
  `create_before_destroy`. An imported credential has no sealed secret and no
  key.
- Only a person can create or change a service account or issue a credential,
  so these fail with the provider's credentials. Reads and imports work. Both
  resource pages say so.
- `host`, `cluster_ca_certificate`, `token`, `client_certificate` and
  `client_key` on the `k0rdent_cluster_kubeconfig` ephemeral resource, parsed
  from the kubeconfig's current context and named as the kubernetes and helm
  providers name their arguments, so a provider block takes each one directly
  instead of decoding `raw` and indexing into its first cluster and user. The
  certificates are PEM, so they take no `base64decode`. `token` and
  `client_key` are sensitive. A kubeconfig whose context can't be chosen, or
  whose user authenticates with something like an exec plugin, opens with a
  warning that says what it found, and every parsed attribute is null, `host`
  included, so no local credential is sent to the platform's cluster. That
  doesn't stop a provider wired from them: the kubernetes and helm providers
  fall back to `KUBE_HOST`, `KUBE_TOKEN`, `KUBE_CONFIG_PATH(S)`, `KUBE_CTX` and
  the rest when `host` is null, and then reach whatever cluster those name.
  The example and the getting-started guide put a postcondition on the
  ephemeral block that stops the run instead. `raw` is unchanged.
- `ssh_access` on `k0rdent_instance_group`, sent on create, and on the
  `k0rdent_instance_group` data source. On a development build of the k0rdent API,
  with the real reservation path wired, a group created without it went `failed`
  with `INVALID_CONFIGURATION` and the same create with one user went
  `active`, so the resource could not create a usable group there before. The
  platform answers the users sorted by username, so the attribute is a set and
  the order written makes no difference. It is immutable, so a change replaces
  the group and needs `allow_replace = true`. A key with whitespace at either
  end is refused at plan time, because the platform trims it and it would read
  back different. Read one from a file with `trimspace(file(...))`.
- `k0rdent_organizations` and `k0rdent_organization`, over `listOrganizations`
  and `getOrganization`, and `k0rdent_instances` and `k0rdent_instance`, over
  `listInstances` and `getInstance`. Each instance carries its
  `network_interfaces`, with the `mac_address` or `guid` a file store grant
  takes to narrow it to one interface, so those two values are no longer
  hardcoded. A project's `organization_id` stays on the hardcoded values guide:
  an API key may list no organizations.
- Cluster upgrades. Pinning `cluster_type` on `k0rdent_kubernetes_cluster` to a
  later `available` version of the same ClusterType now upgrades the cluster in
  place instead of failing the plan. The plan says how many steps it takes, and
  the apply makes one upgrade per version, naming each, waiting each out, and
  skipping any draft or deprecated version between. A step that fails stops the
  apply with state at the version the cluster reached, says the platform will
  not retry from `failed`, and deletes nothing. An earlier version, one that is
  not `available`, or one the ClusterType does not have still fails the plan,
  and so does a route some step cannot take: a BMaaS-only version in the way,
  or a node count a step's bounds do not allow. Node count changes in the
  same apply go before or after each step, wherever the versions' bounds admit
  them, so a version that narrows or raises a pool's bounds can be reached in
  one change. A `cluster_type` known only at apply upgrades too.
- `vpc` on `k0rdent_kubernetes_cluster` and `k0rdent_instance_group`, the VPC
  each brought with it. A peering or binding that names it stays put while the
  cluster changes, where a data source lookup would replace it.
- `docs/walkthrough.md`: zero to a cluster on real hardware, rehearsed against
  the local fake first so the live run tests the deployment rather than the
  walkthrough. Five environment variables are the only difference between them.
- `examples/live` carries a switch for each capability the API supports, and
  a `scorecard` output that answers every capability from the run rather than
  from an opinion. The ones with no API behind them say so, with the
  requirement id, instead of being left out.
- Three guides on the registry pages. Getting started goes from a credential to
  a cluster you can reach in one configuration, and the other two cover
  authenticating a pipeline and importing an existing estate.
- `docs/known-limitations.md`, the places the plan cannot say what the apply
  will do, and what the API does not let a configuration manage at all.
- `docs/support-matrix.md`, which Terraform, OpenTofu and contract versions the
  provider is built and tested against, and which platform versions it is
  supported against, which is still open.
- An air-gapped install through a filesystem mirror in
  `docs/install.md`, with the lock file that is the only check
  `terraform init` makes against a mirror.
- `k0rdent_kubernetes_cluster` data source: a cluster another configuration
  owns, read without adopting it. It carries what the resource cannot without
  drifting: health, the hosted control plane's state, and the current
  operation, so an upgrade in flight shows its target, stage and start time.
- `k0rdent_project` data source: a project another configuration owns, read
  by id without adopting it. `organization_id` is optional: without it the
  organization is looked up from the id.
- `k0rdent_instance_group` data source, with per-pool ready counts and the
  reservation phase.
- `k0rdent_file_store` data source, with what the backing storage enforces for
  each consumer.
- `k0rdent_file_store` resource and `k0rdent_share_types` /
  `k0rdent_share_type` data sources: account-level storage. A store's whole
  lifecycle and the catalog of classes are reachable from Terraform. Storage
  inside a cluster stays a CSI concern, answered by the kubeconfig hand-off
  rather than by a provider surface.
- Deleting a file store detaches its consumers first. The platform refuses to
  delete a store anyone can mount, so a plain destroy would fail. The escape
  hatch the API offers removes the record without contacting the storage
  backend and leaves the data behind, so it is not used and not exposed.
- A file store create that times out at the gateway is recorded and waited
  out when the platform names the store, so a committed store is never left
  untracked.
- An expansion that fails while the store stays `active` fails the apply,
  because the platform reports it only as a reason on the store.
- An instance group whose membership moved, and whose grant the platform did
  not repair by itself, plans an update to the file store that sends the entry
  again. When the platform refuses that re-send, the rest of the change is
  applied and plans stop proposing it until the platform's report changes. A
  group that is gone or now names a different group fails the plan instead of
  being granted again.
- Destroying or replacing a file store warns, naming each consumer whose grant
  is still enforced and the addresses it reaches, with grants still being
  applied listed apart.
- `preferred_export_location` on `k0rdent_file_store` is where a tenant host
  mounts from. `export_locations` is sorted by server and path, so a reordered
  response is not a change.
- `max_capacity_gib` and `state` on the share type data sources, so a
  configuration can see a class's ceiling before it asks for more.
- Lowering `capacity_gib` fails the plan rather than replacing the store.
  Capacity can only grow, and replacing a store to satisfy a smaller number
  would destroy the data it holds.
- A storage class the region does not offer is a plan-time error through
  `k0rdent_share_type`, instead of a rejection partway through the apply.
  Classes are operator-authored and differ between regions.
- `k0rdent_cluster_type_versions` and `k0rdent_cluster_type_version` data
  sources. The history is the nearest thing the platform has to a catalogue of
  supported Kubernetes versions, because a tenant does not pick one: it comes
  with the ClusterType version a cluster binds to. Each entry carries its
  Kubernetes version and a version-scoped `self_link`, which is what pins a
  cluster to an immutable version instead of to whatever the latest becomes.
- `k0rdent_network` and `k0rdent_subnet` data sources, by name. They also read
  the networks and subnets a cluster provisioned, which the resources never
  can.
- `k0rdent_security_group` data source. `owner_kind` and `owner_id` say whether
  a group is a VPC's default and which VPC it may be bound to.
- `k0rdent_vpc_peering` data source, so the side that did not create a peering
  can see whether it is established.
- `k0rdent_vpc_peering` and `k0rdent_vpc_security_groups` resources, and
  `k0rdent_networks`, `k0rdent_subnets`, `k0rdent_address_pools` and
  `k0rdent_address_pool`, `k0rdent_security_groups`, `k0rdent_vpc_peerings` and
  `k0rdent_effective_security_groups` data sources. With these, all 25
  networking operations the API exposes to a tenant are reachable from
  Terraform.
- `k0rdent_effective_security_groups` is the only read that answers what
  actually filters a cluster. Every other surface reports one binding, and what
  reaches an interface is the merge of the owner's groups and each VPC's floor,
  in an order that is precedence rather than presentation.
- `k0rdent_security_group` resource. Rules are `ingress` and `egress` blocks in
  evaluation order, because the platform matches first-match in list order and a
  set would reorder the filter. A port range on a protocol that cannot carry one
  fails the plan instead of the apply.
- `k0rdent_tenant_quota` and `k0rdent_project_quota` data sources. Each
  reports its dimensions keyed by name, with the headroom that binds and the
  scope it binds at, so a module can cap a pool at what the organization or the
  project is actually allowed. A dimension the platform does not enforce reports
  its usage as null rather than 0.
- `k0rdent_network` and `k0rdent_subnet` resources, and `k0rdent_vpcs` and
  `k0rdent_vpc` data sources. This is the VPC and networking half of the tenant
  surface: everything the API offers a tenant is now reachable, and what it
  does not offer is left out rather than faked.
- The fabric kind on a network is a block, not a string. Setting `evpn`,
  `infiniband` or `nvlink` is what decides `type`, so a configuration cannot
  name one fabric and configure another and find out at apply.
- `region` and `project` on both resources, so a resource stays in the scope it
  was created in instead of following the provider block. Changing either
  replaces the resource, and `<region>/<project>/<id>` imports record both.
- A `timeouts` block on both resources, with a separate default for create,
  update and delete. Read does not wait and has none.
- `k0rdent_regions` and `k0rdent_projects` data sources. Region ids and project
  slugs are operator-declared and published nowhere, so filling in the provider
  block meant being told the values.
- `kubernetes_version` on the `k0rdent_cluster_type` data source.
- `k0rdent_cluster_kubeconfig` ephemeral resource, which returns the credential
  without writing it to state.
- `region` and `project` on the `k0rdent_cluster_kubeconfig` data source and
  ephemeral resource, so a cluster outside the provider's scope is reachable.
- A usage example on every resource, data source and ephemeral resource page.
- An SPDX SBOM next to each release archive.
- Acceptance tests: each resource's create, empty second plan, update, import
  in both id forms, and destroy, against the local fake.
- Reproducible registry documentation generation and validation.
- Endpoint-neutral release archives for the supported beta platform matrix.
- Versioned checksum manifests and CI-only checksum signing configuration.
- `k0rdent_project` resource. `organization_id` is an input, because creating
  an organization is not open to a tenant. A destroy waits for the project to
  be gone, and resumes a teardown an earlier destroy left running. Import takes
  `organization/project` or a bare project id.
- Every resource declares schema version 1, and upgrades a state from version
  0, which is what every earlier build wrote, on the first plan. Nothing in the
  state changes but the version. It isn't reversible: an earlier build stops
  on a version 1 state with "Resource instance managed by newer provider
  version". Keep a copy of the state to go back to, as
  `docs/public-compatibility.md` describes.
- A `moved` block from `k0rdentai_kubernetes_cluster` and
  `k0rdentai_instance_group`, the pre-release names, to the current types.
  The documented `terraform state mv` never worked across the rename:
  Terraform refuses it with "resource types don't match". The steps that do
  are in `docs/public-compatibility.md`.
- The error for a cluster that ends `failed`, after a create, a scale, an
  update or an upgrade step, quotes the last five warnings and errors of the
  operation's runtime event log, newest last, with each line's time,
  component and stage. Before, it said only what the cluster's
  `stateMessage` said. Each line is escaped and kept to one line, and a log
  that can't be read leaves the error as it was.
- `kubernetes_version` on `k0rdent_kubernetes_cluster` and the
  `k0rdent_kubernetes_cluster` data source: the Kubernetes version the bound
  ClusterType version carries, read from its `k8s.k0sVersion`. It's
  read-only, because the platform ignores a declared Kubernetes version, and
  a plan that upgrades the cluster shows the version the upgrade reaches. The
  cluster's schema version goes to 2 for it. A version 1 state upgrades on the
  first plan with the attribute null until that plan's refresh fills it, and
  an earlier build stops on a version 2 state, as it does on any newer one.

### Changed

- A `k0rdent_vpc_peering` create reads the far side before it warns, when
  `remote_vpc` is in the peering's own region and project. It lists that VPC's
  peerings, and says nothing when one there points back and is `active`. When
  none points back it says so as a fact, and when the mirror isn't `active`
  yet it names it and its state. An `active` mirror wins over one being
  deleted. A `remote_vpc` anywhere else isn't read, since the provider
  addresses only the region and project it's configured for, and the warning
  says the far side wasn't checked and has to be confirmed out of band. So
  does a read that's refused, now with what the platform answered. The
  warning used to say the mirror wasn't listed by the API at all.
- A file store update the platform refuses with `CONFLICT_IN_USE` because the
  store is still `creating`, as one another run is creating or one tainted by
  a create that timed out can be, waits for the store to settle and is sent
  again, within the update timeout. It goes with the etag the settled store
  reads, unless that read shows something else changed the store, when the
  platform refuses it as a concurrent edit. The same refusal from an `active`
  store is sent once more. It used to fail the apply.
- A cluster upgrade step the platform refuses because another run started an
  operation on the cluster waits for that operation to finish, within the
  update timeout, and plans the step again. It used to fail the apply.
- Setting or removing `allow_replace` on its own is no longer a change to
  the rest of a `k0rdent_kubernetes_cluster`. The plan keeps `node_count` and
  `state` as they are rather than known after apply, and the apply makes no
  request, where it used to read the cluster back and wait for it to settle.
  Which attributes replace a resource, and the refusal naming each one, are
  unchanged. A plan the guard refuses also carries any warning or error the
  resource's own planning raises, since the guard now runs after it.
- Every create names its resource, so a run whose create answer was lost
  converges instead of stopping on `409` or making a second one. With no `id`
  in configuration the provider generates one, `tf-` and 16 hex digits, where
  the platform used to pick a name. When the answer is lost, or a create
  answers `409`, the provider reads the id back and carries on with what it
  finds only when it can show this apply made it: by a create token in the
  resource's tags (cluster, network, subnet, security group, VPC peering), or,
  for a kind without tags (file store, instance group, project), by the id
  being one it generated. Anything else stops with the `terraform import`
  command for it. A cluster, security group, instance group or file store
  whose create answers `504` naming the resource is recorded and waited out.
  When neither the create nor the read back answers, a generated id is
  recorded as tainted so the next run reads it back. A configured id is not,
  since it may name a resource this state does not manage: the apply fails
  with nothing in state, and if the create did land, the next run stops with
  the import command.
  The token travels as the tag `terraform-provider-k0rdent/create-token`,
  which is kept out of state and data sources.
- Growing an instance group to a size known only at apply, such as one read
  from another resource, scales it in place. It used to plan a replacement,
  which releases every host in the group. A shrink known only at apply fails
  the apply before anything changes.
- A `k0rdent_security_group` whose rules come from a `dynamic` block over a
  variable passes `terraform validate`. Validation used to fail with a Value
  Conversion Error on the unknown rule list. A port range on a protocol that
  cannot carry one is still refused once the rules are known.
- A `k0rdent_network` whose fabric block comes from a variable or a conditional
  passes `terraform validate`. It used to fail with a Value Conversion Error.
- Switching a `k0rdent_network` between `evpn` and `infiniband` plans a
  replacement. It used to fail the plan with a Value Conversion Error.
- A `k0rdent_kubernetes_cluster` configured without `node_pool` plans empty
  once applied. It used to propose an update on every plan.
- A 429 with no `Retry-After` is retried with a capped, jittered backoff
  instead of failing the apply. A `Retry-After` longer than 30 seconds is honoured up to the
  operation's deadline, and one past it fails at once naming the wait.
- The supported minimum Terraform version is 1.10, not 1.5. The cluster
  credential is an ephemeral resource, and Terraform recognises those from 1.10.
- A project id starting `proj-` is accepted wherever a project is named. It is
  the form the platform generates for a project created without an id, and it
  used to be refused as an internal identifier.
- `terraform destroy` removes a cluster that is still `creating` or `updating`,
  such as one tainted by a create that timed out. The platform refuses to
  delete a cluster mid-operation, so the destroy cancels the operation with
  `cancelCluster`, warns that it did, and then deletes the cluster, all within
  the delete timeout. It used to fail with a 409 until the platform settled. A
  refused cancel stops the destroy with the platform's answer and keeps the
  cluster in state. Nothing else cancels: a create, update, scale or upgrade
  whose wait times out says the operation is still running and that a destroy
  cancels it. A destroy that finds the cluster already deleting waits for it
  instead of failing. A change whose wait timed out is not cancelled by the
  next apply, which the platform refuses until the cluster is `active` again.
- A wait whose deadline lands during a read reports a timeout, with the advice
  that goes with one, instead of a failed request.
- `k0rdent_cluster_kubeconfig` works in the same apply that creates the
  cluster. The platform issues the kubeconfig shortly after the cluster becomes
  active and answers 404 until then, and both the ephemeral resource and the
  data source failed on that 404. The ephemeral resource now waits while the
  cluster is creating or updating, bounded by a new `timeouts.open` or
  `operation_timeout`, which also bounds a plan when `cluster_id` is already
  known. Both surfaces give an active cluster two minutes to be issued its
  kubeconfig, and they quote the failure reason when the platform recorded one. The
  data source does not wait for a cluster to provision. Both fail at once when
  the cluster does not exist or is being deleted, when the read doesn't reach
  the service, and on a 503.
- A missing `nodePools` array on an instance group keeps the known
  pools rather than planning to destroy them.
- A destroy that cancels a cluster's running operation names that operation
  by the execution id the cluster reports. When
  the cluster has begun another operation since it was read, the destroy
  reads it again and cancels that one, once. A cluster that names no
  operation is reported, with nothing deleted.
- A `503` on a file store create or update, or on a cluster update, is
  retried, since the API says each one changed nothing.
- `global` is accepted as a region. On the all-in-one install compute runs
  under it and it hosts projects, so whether it's a region is the platform's
  to say.
- A cluster whose ClusterType version is later than the configured one fails
  the plan instead of being replaced. The platform now upgrades clusters in
  place, so a cluster upgraded outside Terraform reads back at another version,
  and replacing it would destroy a healthy cluster to undo the upgrade. Pinning
  the version a cluster is already bound to no longer plans a replacement.
- The `k0rdent_cluster_kubeconfig` ephemeral resource reads the cluster's one
  kubeconfig, whose token is minted for the caller, carries the caller's roles
  and lasts about fifteen minutes.
- A cluster scale that the platform refuses for a few seconds while its quota
  accounting settles is retried, so an apply that grows one pool and shrinks
  another no longer fails on its second request.
- Growing an instance group waits, within the `update` timeout, while the
  platform settles its quota accounting or finishes another operation on the
  group, and for up to two minutes while it cannot confirm a pool's size. Each
  of those used to fail the apply with a bare 409 or 503 that would have
  cleared on its own.
- Destroying an instance group whose create timed out waits for the platform
  to cancel the create and tear the group down. It used to fail on the
  `creating` and `failed` states the platform reports along the way.
- An instance-group scale the platform refuses for good says what to do about
  it. A quota refusal names the dimension, what was requested, the most that
  would have been admitted and the scope whose limit binds, for clusters too.
  On a scale both numbers are increases, and on a create they are the new
  resource's node count. A pool that is empty, still provisioning, out of step
  with its reservation or released each get their own next step, and a failed
  or deleting group is reported at once rather than waited on.
- A plan that grows an instance-group pool whose desired size disagrees with
  the hosts it holds warns that the platform will refuse the scale, and gives
  the size that corrects it. It stays quiet while a scale is in flight and when
  the plan replaces the group. The provider does not send that correction
  itself.
- The documentation gives the real reason the provider asks for no scope
  narrowing: the platform accepts the field and ignores it, so a caller asking
  for less privilege would get a full-privilege token.
- A security-group rule's `action`, `protocol`, prefixes and port ranges are
  checked in the plan. Each of them was a 422 arriving partway through an apply
  the user had already approved. So is an EVPN `service_type` and a subnet
  `prefix_length` outside the addressable range; a `prefix_length` outside the
  pool's own narrower bounds still cannot be caught before apply, because a
  validator has no way to read another object.
- A network, subnet or security group the platform owns rather than the tenant
  now fails the plan when something tries to change it. Importing one used to
  give a clean import and a clean plan, then a 409 halfway through the apply.
  Destroying one stops tracking it and says so, rather than being refused: a
  resource adopted by mistake would otherwise be stuck in the state file and
  would block `terraform destroy` for the whole configuration.
- A `k0rdent_vpc_security_groups` destroy plan warns that removing the binding
  reinstates the VPC's DEFAULT group, which permits all egress. The resource
  description said so and a destroy plan renders no provider text.
- A `k0rdent_vpc_security_groups` destroy reads the VPC first and clears the
  binding only if it's still exactly, and in the same order, what the resource
  last read there. A replacement with `create_before_destroy` on the same VPC
  writes the new binding first, and the old one's destroy used to put the VPC
  back on DEFAULT straight after, so the apply ended with the new groups gone.
  It now leaves them, including when they're the same groups, as `-replace`
  writes, and warns. A binding another configuration or the console set since
  the last refresh is left the same way. Writes to the same VPC from the same
  apply wait for the destroy's read and write, so a parallel swap of two
  bindings can't slip between them. A destroy still clears a binding the same
  instance wrote earlier in the run, as a `terraform test` teardown does, since
  each write keeps a token in the instance's private state. The fake now
  refuses a second binding change while one renders, with the 409 the
  contract documents.
- Every acceptance case asserts its resources actually went away. None did, and
  Terraform drops the state entry whether or not Delete reached the platform, so
  a Delete that quietly returned would have passed. The binding resource is
  checked differently, because its destroy is a write: the assertion is that the
  VPC went back to its platform-managed DEFAULT group.
- The local fake can settle an object into `failed` or into a state the
  contract does not declare, and materializes the network, subnet and DEFAULT
  security group a cluster owns. Both branches of every four-set waiter, and
  the platform-ownership guard, had no test that could reach them.
- The local fake models two fabric backends instead of hard-coding `nico` on
  every VPC. Only a `nico` VPC carries security groups, and the provider's
  warning naming that as the cause of a 409 could never fire in a test.
- The local fake answers the way the API does. A missing object is `not found: <kind> "<id>"` with no reason,
  where the fake used to say "no such network" with a reason the platform
  doesn't send. A subnet or VPC peering under a network or VPC that isn't
  there answers the parent's 404. A request under a project or organization
  that doesn't exist is a `403`. In a project being deleted, a
  cluster or file store create is a `409` and every other create is made. A
  network, subnet, security group or peering delete leaves it `deleting`
  until a read finds it gone, and a second delete is a `409`, except for a
  security group, where it's a `204`. A subnet in a network being deleted, and
  a peering on a VPC that isn't settled `active`, are a `409` with
  `CONFLICT_IN_USE`. A security group a VPC carries is refused on delete,
  naming the VPC. The fake serves the organization's
  project list, so the `403` covers it too. The cluster, instance
  group and kubeconfig lists page,
  as the list operations declare, and a cluster's or instance group's node
  pools come back in id order on every read.
- Destroying a `k0rdent_vpc_security_groups` returns the VPC to its default
  group rather than leaving it unfiltered, because an empty list is defined
  upstream as "reinstate the default" and there is no reachable state in which a
  VPC carries no group at all.
- A refused security-group delete now names what is holding it. The upstream
  409 says only that the group is in use, and the holder is usually a VPC
  binding or a cluster somewhere else in the configuration.
- Catalog data sources read every page instead of returning the first one with a
  warning attached.

- A subnet declares every attribute as replacing it, including `display_name`.
  There is no `updateSubnet` upstream, so a rename is a destroy and recreate
  with a new CIDR, and the plan says so rather than the apply failing.

- The endpoint must be `https`, unless its host is a loopback address. The
  credential is sent in the body of the token request, so a cleartext endpoint
  put it on the wire in the clear.
- A redirect is refused rather than followed, for the same reason: a 307 or 308
  replays the request body.
- A cluster reported as `failed` is tolerated for two minutes before the wait
  gives up. The contract calls that state "not a stop condition for a poller",
  and a cluster can return to `active` on its own.
- A lifecycle state the contract does not define now fails immediately, naming
  the value, instead of polling until the operation timeout.
- A resource's `timeouts` block is no longer capped by the provider's
  `operation_timeout`. Raising it above the provider default had no effect, and
  the timeout message named the provider setting whichever one fired.
- A failed wait carries the upstream code and request id, and is redacted, like
  every other API error.
- `poll_interval` below one second against a remote endpoint is now an error.
  The provider does not back off between polls, so a shorter interval is a
  request flood. Loopback is exempt, so the local fake can still be polled fast.
- `region` and `project` must match the contract's slug pattern before they
  reach a request URL. A value such as `..` collapsed the path and sent the
  request somewhere else.
- A `cluster_type` that differs from the platform's answer only in the case of
  the host, or in a default port, or in a `/versions/{id}` the platform
  resolved, is the same binding and no longer replaces the cluster.

### Breaking

- `ids` and `regions` on the `k0rdent_regions` data source include `global`
  wherever it hosts projects, the all-in-one install, even without
  `include_global`. A `for_each` over `ids` there gains an entry. Elsewhere
  `global` is still left out unless `include_global = true`.

- A new `k0rdent_project` needs a region, in the resource or the provider
  block, and the plan stops without one. Changing a project's `region`, or
  the provider region it defaults from, replaces the project, which the
  replacement guard refuses without `allow_replace = true`. An existing
  project needs no change while its configuration leaves `region` out: its
  state upgrades and the next refresh reads its region. A project made before
  regions were required has none and can't be given one, so setting `region`
  on it fails the plan. Migration is in `docs/public-compatibility.md`.
- The data sources read tags and exclude ranges the platform answers as `{}`
  or `[]` as null, the way they read them when the platform left them out.
  The API now always answers them.
  `tags` on `k0rdent_kubernetes_cluster` and `k0rdent_subnet`, and
  `exclude_ranges` on `k0rdent_address_pools`, are null when there are none.

- `limits`, `used` and `remaining` are gone from `k0rdent_tenant_quota`. Read
  `dimensions["<name>"].headroom` and its `scopes` instead: the old maps
  described the organization alone and read an unmeasured usage as 0.
  Migration is in `docs/public-compatibility.md`.
- `id` and `issued_for` are gone from the `k0rdent_cluster_kubeconfig`
  ephemeral resource. A cluster has one kubeconfig and its token is issued to
  whoever opens it, so there is nothing to select and no one else it was
  issued for. Remove them from configuration.
- `download` and `raw` are gone from the `k0rdent_cluster_kubeconfig` data
  source. They wrote a cluster credential into state in plain text. Read the
  body from the ephemeral resource of the same name, which Terraform never
  persists. Migration is in `docs/public-compatibility.md`.
- A change that would replace a cluster, instance group, file store, subnet,
  network or project now fails the plan instead of planning a destroy and a
  create. The error names the attribute that forces the replacement and says
  how to permit it: set `allow_replace = true` on the resource in the same
  configuration as the change. On a subnet that is any change, because the API
  has no update for one. Changes made in place plan as before, and setting or
  removing `allow_replace` on its own changes nothing on the platform. Each
  resource's page lists the attributes that replace it.
- A `timeouts` value of `"0s"` or less now fails the plan. It used to fall
  back to the provider's `operation_timeout` without a word, so a
  configuration that meant "no limit" got 30 minutes. Set a positive
  duration, such as `45m` or `2h`, or leave the value out for the default.

## [0.1.0-beta] - planned, not published

This section is reserved for the first beta release. It is not a tag, an
artifact, or a published provider version.

## Release procedure

[`RELEASE.md`](RELEASE.md) says how to cut a release and what has to be in
place before the first one.
