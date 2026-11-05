# Adaptive Card JSON Schema (test data)

`adaptive-card-1.5.json` is the official Adaptive Card schema, version 1.5 -
the highest card version Microsoft Teams renders. It is used only by
`pkg/card/schema_test.go` to validate the card templates in this repository;
it is not part of the built program.

| | |
|---|---|
| Source | https://github.com/microsoft/AdaptiveCards/blob/93695cf2df1117ab08e36ebe707d0ced18e4e9b3/schemas/1.5.0/adaptive-card.json |
| Commit | `93695cf2df1117ab08e36ebe707d0ced18e4e9b3` (2021-11-15, last change of this file) |
| SHA-256 | `32c91fb285180a266a55b3475dd0396e63fe079311714d3a14963b41f5e17920` |
| License | MIT, Copyright (c) 2017 Microsoft - see `LICENSE-adaptive-card-schema` |

The file is stored unchanged. The schema does not know the Teams extension
`msteams` (for example `"msteams": {"width": "Full"}`) and forbids unknown
properties on the card, so the test removes `msteams` before validating and
checks it separately.

To update: download the file of the new version from the same repository,
pin the commit, update the table above and the file name in the test.
