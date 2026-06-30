---
title: Use with Claude Code
weight: 3
---

This page shows how to bootstrap a devedge service from a single prompt using the devedge
plugin for [Claude Code](https://www.anthropic.com/claude-code). Install the plugin once, then
describe the service you want; the plugin drives the documented scaffold-to-running-service
flow for you. You bring the business domain — the plugin handles the mechanics.

## Install the plugin

Add the devedge marketplace and install the `new-service` plugin. This is a one-time setup;
the plugin is then available in any directory.

```text
/plugin marketplace add infobloxopen/devedge-claude-plugins
/plugin install new-service@devedge
```

## Bootstrap a service

Describe the service you want to build:

```text
build a shopping cart microservice with devedge
```

The `new-service` skill pins the SDK version, scaffolds the project, models your domain (an
owner resource, any child resources, and any custom methods), generates, builds, and runs it,
then round-trips the operations — following the pages in this documentation at each step.

The plugin uses only the published tools and the documented flow, so anything it does you can
also do by hand. To follow the same path yourself, see the [Quickstart](../quickstart/) and
[Define a service](../../how-to/model-and-persist/define-a-service/).

## Update the plugin

Third-party marketplaces do not update automatically. Pull the latest plugin version with:

```text
/plugin marketplace update devedge
/plugin update
```

## Next steps

- [Installation](../installation/) — install the SDK and the codegen tools by hand.
- [Quickstart](../quickstart/) — the manual scaffold-to-running-service flow the plugin automates.
