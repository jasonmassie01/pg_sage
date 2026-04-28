# pg_sage Web UI

React + Vite app embedded into the Go sidecar binary.

## Commands

```bash
npm ci
npm run build
npm test
npm run test:e2e
```

`npm run build` writes `dist/`; the Go build embeds those assets.

## Local Development

```bash
npm run dev
```

The app expects a pg_sage API server. In production and normal local sidecar
runs, the Go binary serves both the API and the compiled UI from the same
origin.

## v0.9 Routes

| Route | Purpose |
|---|---|
| `#/` | Overview |
| `#/cases` | Primary DBA work queue |
| `#/findings` | Compatibility alias for Cases |
| `#/actions` | Pending approval and action history |
| `#/manage-databases` | Fleet management |
| `#/settings` | Settings, emergency controls, Shadow Mode |
| `#/users` | Admin user management |

The UI uses session authentication. Log in through `/api/v1/auth/login`; the
browser stores a `sage_session` cookie.
