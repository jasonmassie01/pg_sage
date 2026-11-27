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

## Routes

Every dashboard route is declared once, in [`src/routes.js`](src/routes.js):
its hash path and old-URL aliases, header title, nav group, palette hint,
tab guide, meme label and the roles that see it. The sidebar, the command
palette (Ctrl/Cmd+K), the tab guide and the meme label are derived from
that list, and [`src/routeRender.jsx`](src/routeRender.jsx) renders the page
for each route. Add a page by adding a route there and a renderer with the
same id; `src/routes.test.js` and `src/routes.app.test.jsx` fail until both
exist.

The nav groups are Operate, Admin (Fleet, Users, Notifications, Alert log,
Settings, MCP tokens, Agents) and a collapsed Advanced group. The role gate
in the UI only hides links: the API enforces every role, and a direct hash
to a page your role cannot open renders Access denied. Press `?` for the
keyboard shortcuts.

The UI uses session authentication. Log in through `/api/v1/auth/login`; the
browser stores a `sage_session` cookie.
