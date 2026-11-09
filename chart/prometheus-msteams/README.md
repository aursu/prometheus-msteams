# prometheus-msteams chart

Deploys [prometheus-msteams](https://github.com/aursu/prometheus-msteams):
Alertmanager notifications to Microsoft Teams channels as Adaptive Cards,
through Teams Workflows.

```bash
helm install prometheus-msteams oci://ghcr.io/aursu/charts/prometheus-msteams \
  --version X.Y.Z -f values.yaml
```

Required: `urlSecret.name`, a Secret you create with one Workflow URL per
key, and at least one entry in `channels` (request path and Secret key).
Every value is described in [values.yaml](./values.yaml); usage and the
card templates in the [project README](https://github.com/aursu/prometheus-msteams#kubernetes).

Tests: `sh chart/test.sh` from the repository root (needs helm).
