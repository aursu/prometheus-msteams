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
  or version. The templates in this repository are checked against the
  Adaptive Card 1.5 schema in the tests.
- Maintained dependencies only: the standard library for HTTP routing and
  logging, the Prometheus client for metrics, sprig v3 for template
  functions. OpenCensus and Jaeger tracing are removed.

## Contents

- [Set up a Teams Workflow](#set-up-a-teams-workflow)
- [Build and run](#build-and-run)
- [Set up Alertmanager](#set-up-alertmanager)
- [Answers to Alertmanager](#answers-to-alertmanager)
- [Several channels](#several-channels)
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

Go 1.26 or later.

```bash
make linux          # bin/prometheus-msteams-linux-amd64
make test           # unit and end-to-end tests, with the race detector
```

Run it with one channel:

```bash
./bin/prometheus-msteams-linux-amd64 \
  -teams-request-uri /alertmanager \
  -teams-incoming-webhook-url "$TEAMS_WORKFLOW_URL" \
  -template-file ./default-message-workflow-card.tmpl
```

Every flag can also be set by an environment variable: the flag name in
upper case with `-` turned into `_` (`-teams-timeout` is `TEAMS_TIMEOUT`).
A flag on the command line wins.

A container image: `make linux docker` builds `prometheus-msteams:<version>`
from the binary and the default template.

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

Set Alertmanager's `--web.external-url`: templates use it (`.ExternalURL`)
for links back to Alertmanager.

## Answers to Alertmanager

Alertmanager retries a notification that is answered 5xx and gives up on
one answered 4xx (it counts it in `alertmanager_notifications_failed_total`).
This service answers:

| Answer | When | Alertmanager |
|---|---|---|
| 200 | Teams accepted the card | done |
| 400 | the request is not an Alertmanager message | gives up |
| 422 | the card cannot be built from the template, or Teams refused it for good: 400 (card rejected), 404 (Workflow deleted), 413 (too large) | gives up |
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
# Paths that use the template given by -template-file.
connectors:
- /high_priority_channel: "https://...workflow URL 1..."
- /low_priority_channel: "https://...workflow URL 2..."

# Paths with a template of their own.
connectors_with_custom_templates:
- request_path: /with_buttons
  template_file: ./examples/templates/default-message-workflow-card-actions.tmpl
  webhook_url: "https://...workflow URL 3..."
  escape_underscores: false  # as -auto-escape-underscores; see Card templates
```

```bash
./bin/prometheus-msteams-linux-amd64 -config-file ./connectors.yaml
```

A request path must start with `/` and must not contain `{`, `}` or a
space. Every URL is checked at startup (`-validate-webhook-url`, on by
default): https, a Workflow host, `/workflows/.../triggers/manual/paths/invoke`
and the query parameters `api-version`, `sp`, `sv` and `sig`.

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

## Card templates

A template is a [Go template](https://pkg.go.dev/text/template), rendered
with [Alertmanager's notification data](https://prometheus.io/docs/alerting/latest/notifications/).
It must define `teams.card` and write the message a Workflow accepts:

```json
{"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive", "content": { ...Adaptive Card... }}]}
```

The output is checked for exactly that before it is sent; the card itself
is passed through unchanged. Teams renders Adaptive Cards up to version
1.5; the designer on [adaptivecards.microsoft.com](https://adaptivecards.microsoft.com/)
helps to build one.

- [default-message-workflow-card.tmpl](./default-message-workflow-card.tmpl):
  the default, used by `-template-file`
- [examples/](./examples): more templates

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

Escape each label or annotation value where the template writes it:

```
"text":  {{ .CommonAnnotations.summary | mdEscape | toJson }},
"url":   {{ .CommonAnnotations.runbook | toJson }},
"url":   {{ printf "%s/#/alerts?filter=%s" .ExternalURL (labelMatchers .CommonLabels | urlquery) | toJson }}
```

`toJson` writes the value as a JSON string, quotes included, so that
quotes and line breaks in it cannot break the card; a link gets no
`mdEscape`. A card that is not valid JSON is not sent (answer 422).

`-auto-escape-underscores` (off by default) is for older templates that
write values unescaped: before rendering, it JSON-escapes every label and
annotation value and turns `_` into `\_`. That also changes values used in
links. Do not combine it with templates that escape themselves, such as
the ones here: the text would show extra backslashes.

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
  -max-idle-conns int
    	The HTTP client maximum number of idle connections (default 100)
  -max-retry-count int
    	How often this service itself retries a failed delivery to Teams. 0 leaves retries to Alertmanager.
  -teams-incoming-webhook-url string
    	The default Microsoft Teams webhook connector.
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

A Helm chart is in [chart/prometheus-msteams](./chart/prometheus-msteams).
It still follows the upstream project (image, MessageCard template) and is
being reworked.
