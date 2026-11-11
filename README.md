# prometheus-msteams

A small web server that receives alert notifications from Prometheus
Alertmanager and posts them to Microsoft Teams channels as Adaptive Cards,
through Teams Workflows ("Post to a channel when a webhook request is
received").

![](./docs/promteams.png)

This is a fork of
[prometheus-msteams/prometheus-msteams](https://github.com/prometheus-msteams/prometheus-msteams).
The main differences:

- **Teams Workflows only.** Office 365 connectors (MessageCard) were
  retired by Microsoft and are removed.
- **Alertmanager learns about failures.** A card that Teams refuses is no
  longer answered 200 and lost: the answer tells Alertmanager whether to
  retry (see [Answers to Alertmanager](#answers-to-alertmanager)).
- **The Workflow URL stays secret.** Its `sig` query parameter is a
  credential. URLs come only from the configuration, never from a request;
  they appear in no error, log line or endpoint. `/config`,
  `/_dynamicwebhook/` and `/debug/pprof` are removed.
- **The card is sent as the template wrote it**, any Adaptive Card field
  or version. The templates in this repository are checked in the tests
  against the Adaptive Card 1.5 schema and the Teams-only elements.
- **A card made for reading under pressure**: the alert name first, the
  group as a table, troubleshooting steps from the rule, buttons to the
  dashboard, the rule, Alertmanager, a silence, the runbook and the graph (see
  [The default card](#the-default-card)).
- Maintained dependencies only: the standard library for HTTP routing and
  logging, the Prometheus client for metrics, sprig v3 for template
  functions. OpenCensus and Jaeger tracing are removed.

## Contents

- [Set up a Teams Workflow](#set-up-a-teams-workflow)
- [Build and run](#build-and-run)
- [Set up Alertmanager](#set-up-alertmanager)
- [Answers to Alertmanager](#answers-to-alertmanager)
- [Several channels](#several-channels)
- [The default card](#the-default-card)
- [Card templates](#card-templates)
- [Metrics and health checks](#metrics-and-health-checks)
- [Flags](#flags)
- [Kubernetes](#kubernetes)

## Set up a Teams Workflow

In the Teams channel: **Workflows**, template **"Post to a channel when a
webhook request is received"**. Copy the URL it shows. It looks like

```
https://<env>.environment.api.powerplatform.com:443/powerautomate/automations/direct/workflows/<id>/triggers/manual/paths/invoke?api-version=1&sp=...&sv=1.0&sig=...
```

or, for older Workflows, a host ending in `.logic.azure.com`.

Set the trigger's **"Who can trigger the flow?"** to **Anyone**. With the
setting for users of your tenant only, a request needs a Microsoft Entra ID
token, which this service does not send; such a URL has no `sig`
parameter, and the service refuses it at startup.

Treat the URL as a password: anyone who has it can post to the channel.

## Build and run

Go 1.27 or later.

```bash
make linux          # bin/prometheus-msteams-linux-amd64
make test           # unit and end-to-end tests, with the race detector
```

Run it with one channel:

```bash
./bin/prometheus-msteams-linux-amd64 \
  -teams-request-uri /alertmanager \
  -teams-incoming-webhook-url-file ./workflow-url \
  -template-file ./default-message-workflow-card.tmpl
```

`./workflow-url` holds the Workflow URL. `-teams-incoming-webhook-url`
takes the URL itself instead, but then it shows in the process list and
the shell history.

Every flag can also be set by an environment variable: the flag name in
upper case with `-` turned into `_` (`-teams-timeout` is `TEAMS_TIMEOUT`).
A flag on the command line wins.

### Container image

```
ghcr.io/aursu/prometheus-msteams:v<major>.<minor>.<patch>
```

Images are published for version tags only, one image tag per release and
no moving tags (no `latest`): a deployment names the version it runs. The
program reports the same version (`-version`, and in the startup log).

The image holds only the program (`/promteams`), the default template
(`/default-message-workflow-card.tmpl`) and the CA certificates, and runs
as user and group 65532, not root. `make docker` builds it locally; the
Dockerfile compiles the program itself.

GitHub Actions ([.github/workflows/ci.yml](./.github/workflows/ci.yml))
test and build every push and pull request. A tag `vX.Y.Z` (or
`vX.Y.Z-rc.N`) publishes the image `:vX.Y.Z`:

```bash
git tag v1.0.0
git push origin v1.0.0
```

### Try it without Alertmanager

```bash
curl -X POST -d @pkg/card/testdata/prom_post_request.json http://localhost:2000/alertmanager
```

## Set up Alertmanager

```yaml
route:
  group_by: ['alertname']
  receiver: 'teams'

receivers:
- name: 'teams'
  webhook_configs:
  - send_resolved: true
    url: 'http://prometheus-msteams:2000/alertmanager'
```

Set Alertmanager's `--web.external-url` to the address people open in a
browser: the card's Alertmanager and Silence buttons are built from it
(`.ExternalURL`). Likewise Prometheus' `--web.external-url`, for the Graph
button (each alert's `GeneratorURL`). Left at their defaults, both are
in-cluster addresses that no browser reaches.

## Answers to Alertmanager

Alertmanager retries a notification that is answered 5xx and gives up on
one answered 4xx (it counts it in `alertmanager_notifications_failed_total`).
This service answers:

| Answer | When | Alertmanager |
|---|---|---|
| 200 | Teams accepted the card | done |
| 400 | the request is not an Alertmanager message | gives up |
| 422 | the card cannot be built from the template (or is above `-max-card-size` even with one alert), or Teams refused it for good: 400 (card rejected), 404 (Workflow deleted), 413 (too large) | gives up |
| 502 | Teams answered 429 or 5xx, or could not be reached | retries |
| 504 | Teams did not answer within `-teams-timeout` | retries |

The answer text, which Alertmanager logs, says what went wrong, without
the Workflow URL.

The service does not retry by itself (`-max-retry-count 0`): a failed
delivery is answered at once and Alertmanager retries it.

## Several channels

Serve several request paths, each posting to its own Workflow, with a
configuration file:

![](./docs/promteams_multiconfig.png)

```yaml
connectors_with_custom_templates:
# The URL from a file; the template given by -template-file.
- request_path: /high_priority_channel
  webhook_url_file: /etc/teams/high
# A template of its own.
- request_path: /compact
  webhook_url_file: /etc/teams/compact
  template_file: ./templates/compact.tmpl
  escape_underscores: false  # as -auto-escape-underscores; see Card templates

# The URL written into this file, with the template given by
# -template-file. Keep this for files that are themselves secret.
connectors:
- /low_priority_channel: "https://...workflow URL..."
```

```bash
./bin/prometheus-msteams-linux-amd64 -config-file ./connectors.yaml
```

Per entry of `connectors_with_custom_templates`: `request_path`, the URL
as `webhook_url_file` (a file holding it) or `webhook_url` (the URL itself),
and optionally `template_file` and `escape_underscores`.

URL files are read once, at startup: after a URL changes, restart the
service. Spaces and line breaks around the URL are dropped.

A request path must start with `/` and must not contain `{`, `}` or a
space. Every URL is checked at startup (`-validate-webhook-url`, on by
default): https, a Workflow host, `/workflows/.../triggers/manual/paths/invoke`
and the query parameters `api-version`, `sp`, `sv` and `sig`. Errors name
the request path and the file, never the URL.

In Alertmanager, route to the paths:

```yaml
route:
  receiver: 'low'
  routes:
  - receiver: 'high'
    matchers: ['severity="critical"']

receivers:
- name: 'high'
  webhook_configs:
  - send_resolved: true
    url: 'http://prometheus-msteams:2000/high_priority_channel'
- name: 'low'
  webhook_configs:
  - send_resolved: true
    url: 'http://prometheus-msteams:2000/low_priority_channel'
```

## The default card

[default-message-workflow-card.tmpl](./default-message-workflow-card.tmpl),
one card per notification (one alert group):

```
[icon] HighMemoryLoad ×3                      the alert name; ×N for a group
       Env: prod · Cluster: prod-1 · Namespace/web    where it happened
       FIRING  critical  3 alerts             status, severity, count
Memory usage above 90%                        summary, once
Swap is in use on the node.                   description
Started 20:34 · firing for 10 min             resolved: Lasted 13 min · 20:34 – 20:47
job        node-exporter                      the labels worth reading
> Affected instances (3)                      a group, collapsed: one row per alert
> Troubleshooting       access · check · mitigate     collapsed
> All labels and annotations                 8        collapsed
[Dashboard] [Alert rule] [Alertmanager] [Silence] [Runbook] [Graph]
```

- **Colours**: red for critical, orange for warning, blue otherwise, green
  when resolved.
- **Where** - the line under the alert name - says what the alert is
  about, general to specific, from these labels when the alert has them
  (for a group, when all its alerts share them). Context reads
  `Title: value`; Kubernetes objects read `Kind/name`, as kubectl names
  them:

  | Part | Label | Shown as |
  |---|---|---|
  | datacenter | `datacenter` | `IDC: eu-1` |
  | project | `project` | `Project: shop` |
  | environment | `environment`, else `env` | `Env: prod` |
  | cluster | `cluster` | `Cluster: prod-1` |
  | host | `host` | `Host: web1.example.org` |
  | namespace | `namespace` | `Namespace/web` |
  | object | `object` (e.g. `Service/www`), else the first Kubernetes label of `deployment`, `statefulset`, `daemonset`, `cronjob`, `job_name`, `persistentvolumeclaim`, `horizontalpodautoscaler`, `pod`, `node` | `Deployment/api` |

  A Kubernetes alert reads `Env: prod · Cluster: prod-1 · Namespace/web ·
  Deployment/api`; an alert about a server `IDC: eu-1 · Project: shop · Env:
  stage · Host: web1.example.org`. It comes first because a reader does not
  always see the channel: the Teams activity feed shows the card without
  it. Rules that know more than their labels say - where a probed site
  runs, the Service behind it - set these labels themselves (`labels` of
  the rule, or of the probe target).
- **Times** are written as the Adaptive Card functions `{{TIME()}}` and
  `{{DATE()}}`: Teams shows them in the reader's time zone. The date is
  added when the alert started more than 12 hours before the card.
- **Facts** are the alert's labels (for a group, the labels all its alerts
  share), except those shown elsewhere or of no use to a reader:
  `alertname`, `severity`, the labels of the where line, `prometheus`,
  `endpoint`, `container`, `service`. The full set is under "All labels and
  annotations".
- **A group** lists its alerts in a collapsed section, as a table that
  scrolls past about six rows - also on a resolved card. The first column
  is the first of the labels `instance`, `pod`, `name`, `job`, `container`
  that the alerts do not all share, and it names the section with the
  count: "Affected pods (12)"; "Alerts (2)" when none of them differs. When
  the alerts have different summaries, each row shows its own.
- **Buttons**: Dashboard (annotation `dashboard_url`: the dashboard of what
  the alert is about), Alert rule (annotation `alert_url`: where the rule
  is defined, e.g. in Grafana), Alertmanager (the group's alerts), Silence
  (a new silence with the alert's labels, for a group the group's), Runbook
  (annotation `runbook_url`, or `runbook`), Graph (the rule's expression in
  Prometheus). A group gets only the links its alerts share; when each
  alert has its own `dashboard_url` (one per probed site, say), the table
  links each row to it instead. A resolved card keeps Dashboard, Alert rule
  and Graph.
- **Teams mobile** does not render every element (icons, badges, code
  blocks); each of them carries a fallback - plain text, or nothing for the
  icon - so the card still reads there.

To change what the card shows, copy the template: the lists of hidden
labels, where-line labels and their titles, Kubernetes object labels and
row labels are at its top.

### Troubleshooting in the rule

A firing card gets a collapsed **Troubleshooting** section when the rule
has any of these annotations:

| Annotation | Shown as | Write |
|---|---|---|
| `access` | text (markdown) | where to look (dashboards, UIs) and how to reach the system |
| `check` | code, with a Copy button | shell commands, read-only, that confirm or rule out the problem in a minute |
| `mitigate` | text (markdown) | how to stop the damage: roll back, fail over, tell the people affected; the root cause comes later |
| `runbook_url` | a link, and the Runbook button | the full playbook, when there is more to say |

This follows the on-call practice of the Google SRE books: every alert
should need an action, a playbook entry shortens the time to recover, and
mitigation comes before the root cause.

```yaml
- alert: EndpointDown
  expr: probe_success{job="blackbox"} == 0
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.instance }} does not answer"
    access: |
      - Dashboard: https://grafana.example.org/d/endpoints
      - Probe history: https://prometheus.example.org/graph?g0.expr=probe_success
    check: |
      curl -sS -o /dev/null -w '%{http_code} %{time_total}s\n' {{ $labels.instance }}
      dig +short {{ reReplaceAll "^https?://([^/:]+).*" "$1" $labels.instance }}
    mitigate: |
      1. Check from outside your network: is it the site or the probe path?
      2. A deploy, DNS or certificate change in the last hour? Roll it back.
      3. Tell the people affected that you are on it.
    runbook_url: https://runbooks.example.org/endpoint-down
```

Prometheus fills in the labels when the alert fires, so the commands on
the card are ready to run. `access` and `mitigate` are markdown written by
you and are not escaped; escape `_` and `*` yourself where they must show
as they are. In a group whose alerts have different steps, the card shows
the first alert's and says so.

## Card templates

A template is a [Go template](https://pkg.go.dev/text/template), rendered
with [Alertmanager's notification data](https://prometheus.io/docs/alerting/latest/notifications/).
It must define `teams.card` and write the message a Workflow accepts:

```json
{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive", "content": { ...Adaptive Card... }}]}
```

The output is checked for exactly that before it is sent; the card itself
is passed through unchanged. Teams renders Adaptive Cards up to version
1.5, plus elements of its own (Badge, Icon, CodeBlock, ...); the designer
on [adaptivecards.microsoft.com](https://adaptivecards.microsoft.com/)
helps to build one. Give every Teams-only element a `fallback` for Teams
mobile; the tests require one in the templates here.

The simplest way to write a card is the default template's: build it as
data with `dict` and `list` and write it once with `toJson`, which escapes
every value.

- [default-message-workflow-card.tmpl](./default-message-workflow-card.tmpl):
  the default, used by `-template-file`

Functions available in templates:

- Alertmanager's own (`toUpper`, `reReplaceAll`, `safeHtml`, ...)
- the [sprig](https://masterminds.github.io/sprig/) functions, except `env`
  and `expandenv`: a template cannot read the environment of the process
- `toYaml`, `fromYaml`, as in Helm
- `counter`: `{{ $c := counter }}{{ call $c }}` gives 0, 1, 2, ...
- `mdEscape`: escapes `\`, `*`, `_`, `[` and `]`, so that Teams' markdown
  shows a value as it is (`high_memory_load`, not italics)
- `labelMatchers`: labels as an Alertmanager filter,
  `{alertname="HighLoad", severity="warning"}`, for links to Alertmanager
- `urlComponent`: escapes a part of a link as JavaScript's
  `encodeURIComponent` does, a space as `%20`. Use it, not `urlquery`, for
  Alertmanager's filter: it is read from the part after `#`, where the `+`
  that `urlquery` writes for a space stays a `+`
- `shortDuration`: a duration to the minute, `10 min`, `2 h 5 min`, `3 d 4 h`
- `now` and `since`, as in Alertmanager: `{{ since .StartsAt | shortDuration }}`

Escape each label or annotation value where the template writes it:

```
"text":  {{ .CommonAnnotations.summary | mdEscape | toJson }},
"url":   {{ .CommonAnnotations.runbook | toJson }},
"url":   {{ printf "%s/#/alerts?filter=%s" .ExternalURL (labelMatchers .CommonLabels | urlComponent) | toJson }}
```

`toJson` writes the value as a JSON string, quotes included, so that
quotes and line breaks in it cannot break the card; a link gets no
`mdEscape`. A card that is not valid JSON is not sent (answer 422).

`-auto-escape-underscores` (off by default) is for older templates that
write values unescaped: before rendering, it JSON-escapes every label and
annotation value and turns `_` into `\_`. That also changes values used in
links. Do not combine it with templates that escape themselves, such as
the ones here: the text would show extra backslashes.

### Large alert groups

Teams refuses a message above about 28 KB. A card larger than
`-max-card-size` (24 KiB by default) is built again with fewer alerts: as
many as fit, firing ones first. The number left out is added to
`.TruncatedAlerts`, the field Alertmanager also sets when its webhook
`max_alerts` cuts a group; the default template then counts them in the
title and adds "N more alerts not shown" with a link to the group in
Alertmanager. If not
even one alert fits, nothing is sent and Alertmanager is answered 422.
Each shortened card is logged as a warning ("card too large, alerts left
out").

## Metrics and health checks

| Path | |
|---|---|
| `GET /metrics` | Prometheus metrics |
| `GET /-/healthy`, `GET /-/ready` | 200 while the process runs; the configuration is read once at startup, and an invalid one stops it |

Metrics:

| Metric | Labels | |
|---|---|---|
| `prometheus_msteams_requests_total` | `handler`, `code` | requests from Alertmanager by request path and answer |
| `prometheus_msteams_request_duration_seconds` | `handler` | time to answer, Teams included |
| `prometheus_msteams_teams_requests_total` | `code` | requests to Teams by Teams' answer, every retry included |
| `prometheus_msteams_teams_request_duration_seconds` | | time until Teams answered |

plus the Go runtime and process metrics.

## Flags

```
  -auto-escape-underscores
    	Before rendering, JSON-escape every label and annotation value and replace '_' with '\_'. Only for templates that do not escape values themselves.
  -config-file string
    	The connectors configuration file.
  -debug
    	Set log level to debug mode.
  -http-addr string
    	HTTP listen address. (default ":2000")
  -idle-conn-timeout duration
    	The HTTP client idle connection timeout duration. (default 1m30s)
  -insecure-skip-verify
    	Disable validation of the server certificate.
  -log-format string
    	json|fmt (default "json")
  -max-card-size int
    	The largest card sent to Teams, in bytes. A larger card shows only as many alerts as fit and says how many are left out. 0 means no limit. (default 24576)
  -max-idle-conns int
    	The HTTP client maximum number of idle connections (default 100)
  -max-retry-count int
    	How often this service itself retries a failed delivery to Teams. 0 leaves retries to Alertmanager.
  -teams-incoming-webhook-url string
    	The default Microsoft Teams webhook connector.
  -teams-incoming-webhook-url-file string
    	A file holding the Teams Workflow URL for -teams-request-uri, instead of -teams-incoming-webhook-url.
  -teams-request-uri string
    	The default request URI path where Prometheus will post to.
  -teams-timeout duration
    	How long one delivery to Teams may take, retries included. 0 means no limit. (default 30s)
  -template-file string
    	The Adaptive Card template file. (default "./default-message-workflow-card.tmpl")
  -tls-handshake-timeout duration
    	The HTTP client TLS handshake timeout. (default 30s)
  -validate-webhook-url
    	Check that every webhook URL looks like a Teams Workflow URL (https, Workflow host, .../workflows/.../triggers/manual/paths/invoke, api-version, sp, sv and sig present). (default true)
  -version
    	Print the version
```

Accepted and ignored, so that existing command lines still start:
`-workflow-webhook`, `-jaeger-trace`, `-jaeger-agent`.

Logs go to stderr. The full alert and the rendered card are logged with
`-debug` only.

## Kubernetes

The Workflow URLs go into a Secret, one key per channel, which the pod
mounts as files; the configuration, which then holds no secret, goes into a
ConfigMap. The Secret is yours to create (kubectl, External Secrets, Sealed
Secrets, ...): the chart below takes only its name, so no URL is ever in
Helm values or in git.

### Helm chart

Published with every release, in the same version as the image (chart
`X.Y.Z` deploys image `vX.Y.Z`):

```bash
helm install prometheus-msteams oci://ghcr.io/aursu/charts/prometheus-msteams \
  --version X.Y.Z -f values.yaml
```

```yaml
# values.yaml
urlSecret:
  name: prometheus-msteams-urls     # keys "critical" and "warnings"
channels:
- path: /critical                   # Alertmanager posts to http://<service>:2000/critical
  urlKey: critical
- path: /warnings
  urlKey: warnings
  template: compact                 # optional, from "templates"
templates:
  compact: |
    {{ define "teams.card" }}
    ...
    {{ end }}
serviceMonitor:
  enabled: true
```

All values, with comments: [chart/prometheus-msteams/values.yaml](./chart/prometheus-msteams/values.yaml).
The pod runs as 65532 with a read-only root file system; a changed
configuration or template replaces the pods by itself, a changed Secret
needs a restart (see below).

### Without the chart

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: prometheus-msteams-urls
type: Opaque
stringData:                  # one key per Workflow; each key becomes a file
  high: "https://...workflow URL 1..."
  low: "https://...workflow URL 2..."
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-msteams-config
data:
  connectors.yaml: |
    connectors_with_custom_templates:
    - request_path: /high_priority_channel
      webhook_url_file: /etc/teams/high
    - request_path: /low_priority_channel
      webhook_url_file: /etc/teams/low
---
# In the Deployment's pod template:
spec:
  securityContext:
    fsGroup: 65532           # the image's group; the Secret files get it
  containers:
  - name: prometheus-msteams
    image: ghcr.io/aursu/prometheus-msteams:v1.0.0
    args:
    - -config-file=/etc/config/connectors.yaml
    volumeMounts:
    - name: config
      mountPath: /etc/config
    - name: urls
      mountPath: /etc/teams
      readOnly: true
  volumes:
  - name: config
    configMap:
      name: prometheus-msteams-config
  - name: urls
    secret:
      secretName: prometheus-msteams-urls
      defaultMode: 0440      # owner root, group fsGroup: readable by the service only
```

The service does not run as root, so the Secret files must be readable
by its group: `fsGroup` with mode `0440`. With `0400` the files belong to
root alone and the service stops at startup with "permission denied".

Kubernetes updates the mounted files when the Secret changes, but the
service reads them only at startup: after changing a URL, restart it
(`kubectl rollout restart deployment/prometheus-msteams`).
