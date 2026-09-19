package pubsub

// Help is the integration guide printed by `appboss pubsub help` and shown in the console. It is
// the same text for both so the browser and the terminal never drift.
const Help = `PubSub channels

Enable it in the app's appboss.yaml (or in defaults: of the host file):

    pubsub:
      path: /socketio      # empty disables it
      # secret: $PUBSUB_SECRET   # empty generates one per app under state_dir
      # replay: 10
      # max_clients: 500
      # max_message_size: 64k
      # client_events: true
      # test: false

Routes on the app's own hosts:

    GET  <path>/<channel>        Upgrade: websocket        subscribe (WebSocket)
    GET  <path>/<channel>        Accept: text/event-stream subscribe (SSE)
    POST <path>/<channel>        Authorization: Bearer ... publish
    GET  <path>/client.js                                  browser client library
    GET  <path>/_test                                      self-test, only when test: true

Publish over HTTP (any language, any process):

    curl -X POST https://myapp.example.com/socketio/chat \
      -H "Authorization: Bearer $PUBSUB_SECRET" \
      -H "Content-Type: application/json" \
      -d '{"event":"message","data":{"text":"hello"}}'

    # A body without an event/data envelope becomes the data of a "message" event.
    curl -X POST https://myapp.example.com/socketio/chat \
      -H "Authorization: Bearer $PUBSUB_SECRET" -d 'plain text'

Subscribe in the browser with the bundled client. It reads the path from the directory it was
served from, so connect() needs no arguments:

    <script src="/socketio/client.js"></script>
    <script>
      const chat = Pubsub.connect().channel('chat');
      chat.on('message', (envelope) => console.log(envelope.event, envelope.data));
      chat.on('open', () => chat.send('typing', { user: 'a' }));   // WebSocket only
      chat.on('close', () => {});
      chat.on('error', (err) => {});
    </script>

Subscribe without the library:

    // WebSocket
    const ws = new WebSocket('wss://myapp.example.com/socketio/chat');
    ws.onmessage = (event) => console.log(JSON.parse(event.data));

    // SSE
    const es = new EventSource('/socketio/chat');
    es.onmessage = (event) => console.log(JSON.parse(event.data));

Notes:

  * The hub is independent of the app process: subscribers connect while the app is stopped, and
    realtime traffic never wakes it.
  * Each message is { "event": string, "data": any, "ts": RFC3339, "replay": true? }. The last
    ` + "`replay`" + ` messages are replayed to a subscriber that joins late, oldest first.
  * Channels are one path segment; client.js, _test and _selftest are reserved.
  * A slow subscriber is dropped rather than blocking the publisher. The limit is max_clients.
  * The publish secret comes from the config or is generated under state_dir. Read it with
    ` + "`appboss pubsub`" + `, replace it with ` + "`appboss pubsub rotate`" + `.`
