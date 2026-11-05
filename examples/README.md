# Card templates

A card template is a Go template, rendered with Alertmanager's notification
data, that writes the message a Teams Workflow accepts: an Adaptive Card
inside `{"type": "message", "attachments": [...]}`. The template must define
`teams.card`. Pass it with `-template-file`, or per request path in the
configuration file (`connectors_with_custom_templates`).

Every template here, and the default one, is checked by the tests against
the Adaptive Card 1.5 schema, for firing and resolved alerts.

## default-message-workflow-card-actions.tmpl

[The default card](../default-message-workflow-card.tmpl) plus buttons:

- **View in Alertmanager**: opens Alertmanager filtered to the alert
  group's common labels; set `--web.external-url` in Alertmanager, it
  becomes `.ExternalURL`
- **View in Alertkarma**: shown when `$alertkarma_prefix` at the top of the
  template is set
- **Investigate**: from the annotation `investigate_link`
- **runbook**: from the annotation `runbook`

Turn all buttons off with `$should_have_potential_actions := false`.
