const port = Number(Bun.argv[2]);
// Shows which environment layer won: .env beats env: in dboss.yaml, and PORT is always dboss's.
const env = (name) => Bun.escapeHTML(process.env[name] ?? "unset");

Bun.serve({
  hostname: "127.0.0.1",
  port,
  routes: {
    "/": new Response(
      `<!doctype html>
      <html lang="en">
        <head>
          <meta charset="utf-8">
          <meta name="viewport" content="width=device-width, initial-scale=1">
          <title>Bun service</title>
        </head>
        <body>
          <main>
            <h1>Hello from Bun</h1>
            <p>Served by dboss on port ${port}.</p>
            <p>PROC_TYPE=${env("PROC_TYPE")} GREETING=${env("GREETING")} SOURCE=${env("SOURCE")} PORT=${env("PORT")}</p>

            <h2>Static files</h2>
            <img src="/dboss.svg" alt="Served by dboss" width="260" height="72" style="max-width: 100%; height: auto;">
            <p>This image is served by dboss straight from disk, not by the Bun app.</p>
            <p>The file is <code>public/dboss.svg</code>; dboss answers <code>/dboss.svg</code> at the same path, so the app never sees the request and the asset still loads while the app is stopped.</p>
          </main>
        </body>
      </html>`,
      {headers: {"content-type": "text/html; charset=utf-8"}},
    ),
    "/up": Response.json({service: "bun", status: "ok"}),
    // Target of the host notify webhook, so dboss events show up in this app's stdout.
    "/notify": {
      POST: async (request) => {
        console.log(`notify ${await request.text()}`);
        return new Response(null, {status: 204});
      },
    },
  },
});

console.log(`Bun service listening on http://127.0.0.1:${port}`);
