---
title: Simple Host Enterprise authentication
canonical: {{BASE_URL}}/auth.md
---

# Authentication

Sign in through your company's OIDC provider at [Sign in]({{BASE_URL}}/auth/login). Browser sessions are bound to the host where they were issued.

## MCP connector

Connect to {{BASE_URL}}/mcp. The connector uses OAuth authorization code with PKCE. Complete company sign-in and approve the connector on the browser consent page. Its access token is accepted only on /mcp. Manage connected apps and revoke access on the [dashboard]({{BASE_URL}}/dashboard).

## CI keys

A signed-in person mints keys on the dashboard. Send a key in X-API-Key. The default publish scope permits deployment; full permits management; offboard permits only POST /api/admin/users/disable. Keys cannot mint more keys. Admin routes otherwise require a browser session.

Every key expires. This installation permits at most {{API_KEY_MAX_DAYS}} days, with an absolute ceiling of 365 days. Choose the shortest lifetime and narrowest scope your job needs. The server checks the person's current access and team membership on requests.

See the [API contract]({{BASE_URL}}/openapi.yaml) and [skill bundle]({{BASE_URL}}/skills.zip).
