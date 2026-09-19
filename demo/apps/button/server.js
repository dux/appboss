const port = Number(Bun.argv[2]);

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
          <title>Button service</title>
        </head>
        <body>
          <main>
            <h1>Hello from the button app</h1>
            <p>Started by an operator click, not by a crawler. Port ${port}.</p>
          </main>
        </body>
      </html>`,
      {headers: {"content-type": "text/html; charset=utf-8"}},
    ),
    "/up": Response.json({service: "button", status: "ok"}),
  },
});

console.log(`Button service listening on http://127.0.0.1:${port}`);
