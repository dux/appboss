// app-boss pubsub client. No dependencies. Serve it from the app's own host and use:
//
//   const bus = Pubsub.connect({ path: '/socketio' });
//   const chat = bus.channel('chat');
//   chat.on('message', (envelope) => console.log(envelope.event, envelope.data));
//   chat.on('open', () => chat.send('typing', { user: 'a' }));   // WebSocket only
//   chat.on('close', () => {});
//   chat.on('error', (err) => {});
//
// Lifecycle events 'open', 'close' and 'error' are reserved; every other name is a server event.
// connect({ transport: 'ws' | 'sse' | 'auto' }) forces a transport. 'auto' uses a WebSocket and
// falls back to SSE if the socket cannot open.
(function (global) {
  'use strict';

  function normalize(path) {
    if (!path) return '/socketio';
    if (path.charAt(0) !== '/') path = '/' + path;
    return path.replace(/\/+$/, '');
  }

  function Client(options) {
    this.path = normalize(options.path);
    this.origin = options.origin || (global.location ? global.location.origin : '');
    this.transport = options.transport || 'auto';
    this.channels = {};
  }

  Client.prototype.channel = function (name, options) {
    if (!this.channels[name]) this.channels[name] = new Channel(this, name, options || {});
    return this.channels[name];
  };

  Client.prototype.close = function () {
    var self = this;
    Object.keys(this.channels).forEach(function (name) { self.channels[name].close(); });
  };

  function Channel(client, name, options) {
    this.client = client;
    this.name = name;
    this.transport = options.transport || client.transport;
    this.handlers = {};
    this.closed = false;
    this.attempt = 0;
    this.opened = false;
    this.conn = null;
    this.open();
  }

  Channel.prototype.on = function (event, fn) {
    (this.handlers[event] = this.handlers[event] || []).push(fn);
    return this;
  };

  Channel.prototype.off = function (event, fn) {
    var list = this.handlers[event] || [];
    var index = list.indexOf(fn);
    if (index >= 0) list.splice(index, 1);
    return this;
  };

  Channel.prototype.emit = function (event, payload, envelope) {
    var list = this.handlers[event] || [];
    for (var i = 0; i < list.length; i++) {
      try { list[i](payload, envelope); } catch (e) {}
    }
  };

  Channel.prototype.dispatch = function (envelope) {
    this.emit('message', envelope, envelope);
    if (envelope && typeof envelope.event === 'string') this.emit(envelope.event, envelope.data, envelope);
  };

  Channel.prototype.url = function () {
    return this.client.origin + this.client.path + '/' + encodeURIComponent(this.name);
  };

  Channel.prototype.open = function () {
    var useSSE = this.transport === 'sse' || (this.transport === 'auto' && typeof global.WebSocket === 'undefined');
    if (useSSE) this.openSSE(); else this.openWebSocket();
  };

  Channel.prototype.openWebSocket = function () {
    var self = this;
    var socket;
    try {
      socket = new global.WebSocket(this.url().replace(/^http/, 'ws'));
    } catch (e) {
      this.fallback();
      return;
    }
    this.conn = socket;
    socket.onopen = function () { self.opened = true; self.attempt = 0; self.emit('open'); };
    socket.onmessage = function (event) {
      var envelope;
      try { envelope = JSON.parse(event.data); } catch (e) { return; }
      self.dispatch(envelope);
    };
    socket.onerror = function () { self.emit('error', new Error('websocket error')); };
    socket.onclose = function () {
      self.conn = null;
      self.emit('close');
      if (!self.closed) self.reconnect();
    };
  };

  Channel.prototype.openSSE = function () {
    var self = this;
    if (typeof global.EventSource === 'undefined') {
      this.emit('error', new Error('sse unsupported'));
      return;
    }
    var source = new global.EventSource(this.url());
    this.conn = source;
    source.onopen = function () { self.opened = true; self.attempt = 0; self.emit('open'); };
    source.onmessage = function (event) {
      var envelope;
      try { envelope = JSON.parse(event.data); } catch (e) { return; }
      self.dispatch(envelope);
    };
    source.onerror = function () { self.emit('error', new Error('event source error')); };
  };

  Channel.prototype.reconnect = function () {
    var self = this;
    if (this.closed) return;
    if (this.transport === 'auto' && !this.opened) { this.fallback(); return; }
    var delay = Math.min(30000, 1000 * Math.pow(2, this.attempt++));
    global.setTimeout(function () { if (!self.closed) self.open(); }, delay);
  };

  // fallback switches an unopened auto channel to SSE, e.g. when a proxy blocks the upgrade.
  Channel.prototype.fallback = function () {
    this.transport = 'sse';
    this.openSSE();
  };

  Channel.prototype.send = function (event, data) {
    if (!this.conn || this.conn.readyState !== 1 || typeof this.conn.send !== 'function') return false;
    this.conn.send(JSON.stringify({ event: event, data: data === undefined ? null : data }));
    return true;
  };

  Channel.prototype.close = function () {
    this.closed = true;
    if (!this.conn) return;
    try {
      if (typeof this.conn.close === 'function') this.conn.close();
    } catch (e) {}
    this.conn = null;
  };

  global.Pubsub = { connect: function (options) { return new Client(options || {}); }, version: '1' };
})(typeof window !== 'undefined' ? window : this);
