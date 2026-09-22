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
