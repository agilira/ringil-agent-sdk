# Ringil app

React, Vite and strict TypeScript. All API requests use the session cookie on the same origin; no browser API tokens, CDN assets, analytics or mock data.

From `app/`:

```sh
npm ci
npm run build
npm test
npx playwright install chromium
npm run e2e
```

Point the host configuration at the absolute build directory:

```yaml
serve:
  app: /absolute/path/to/ringil-agent-sdk/app/dist
```

Build the CLI from its separate module (`cd cmd/ringil && go build -o /tmp/ringil .`), run `ringil serve`, and open the one-time sign-in link it prints. For a new link use `ringil serve link`. Serve the app through Ringil, including during development, so authentication and the Content Security Policy are exercised. `npm run dev` is suitable for editing assets, but does not add an authenticated API proxy.

E2E starts a real server on `127.0.0.1:18765` with isolated temporary config/data/work folders. It reads `NVIDIA_API_KEY` from the server environment or the repository root `.env`, uses `nvidia:nvidia/nemotron-3.5-lightning-30b-a3b`, and runs serially. No key is passed to the browser or printed. Traces, test screenshots and videos are disabled. Free-tier rate limits may require waiting before another run. Never run the model tests against a server with access to sensitive work files.

English and Italian follow browser language initially. Settings stores the language and light/dark/system preference locally.
