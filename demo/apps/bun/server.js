const port = Number(Bun.env.PORT ?? 3000);

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
            <p>Served by deploy-boss on port ${port}.</p>
          </main>
        </body>
      </html>`,
      {headers: {"content-type": "text/html; charset=utf-8"}},
    ),
    "/up": Response.json({service: "bun", status: "ok"}),
  },
});

console.log(`Bun service listening on http://127.0.0.1:${port}`);
