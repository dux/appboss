const port = Number(Bun.argv[2]);

Bun.serve({
  hostname: "127.0.0.1",
  port,
  routes: {
    "/": (request) => new Response(
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
            <p>Signed in as ${Bun.escapeHTML(request.headers.get("x-dboss-user") ?? "nobody")}.</p>
            <p><a href="/chat">Live chat over dboss pubsub</a></p>
          </main>
        </body>
      </html>`,
      {headers: {"content-type": "text/html; charset=utf-8"}},
    ),
    // Uses the dboss pubsub hub on /socketio: client.js subscribes to the lobby channel and sends
    // over the WebSocket (client_events). Publish from a shell with the secret from `dboss pubsub`:
    //   curl -X POST http://button.lvh.me/socketio/lobby -H "Authorization: Bearer <secret>" \
    //     -d '{"event":"message","data":{"from":"curl","text":"hi"}}'
    "/chat": (request) => new Response(
      `<!doctype html>
      <html lang="en">
        <head>
          <meta charset="utf-8">
          <meta name="viewport" content="width=device-width, initial-scale=1">
          <title>Lobby chat</title>
          <script src="/socketio/client.js"></script>
        </head>
        <body>
          <main>
            <h1>Lobby</h1>
            <p id="status">connecting...</p>
            <form id="form"><input id="text" autocomplete="off" placeholder="Say something"> <button>Send</button></form>
            <ul id="log"></ul>
          </main>
          <script>
            const me = ${JSON.stringify(request.headers.get("x-dboss-user") ?? "anonymous")};
            const lobby = Pubsub.connect().channel("lobby");
            const status = document.getElementById("status");
            lobby.on("open", () => { status.textContent = "connected as " + me; });
            lobby.on("close", () => { status.textContent = "disconnected"; });
            lobby.on("message", ({data, replay}) => {
              const item = document.createElement("li");
              // A plain-text publish arrives as a bare string.
              const line = typeof data === "string" ? data : (data.from ?? "?") + ": " + data.text;
              item.textContent = (replay ? "(earlier) " : "") + line;
              document.getElementById("log").prepend(item);
            });
            document.getElementById("form").addEventListener("submit", (event) => {
              event.preventDefault();
              const input = document.getElementById("text");
              if (input.value) lobby.send("message", {from: me, text: input.value});
              input.value = "";
            });
          </script>
        </body>
      </html>`,
      {headers: {"content-type": "text/html; charset=utf-8"}},
    ),
    "/up": Response.json({service: "button", status: "ok"}),
  },
});

console.log(`Button service listening on http://127.0.0.1:${port}`);
