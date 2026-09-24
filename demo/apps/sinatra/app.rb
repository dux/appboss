# frozen_string_literal: true

require "fileutils"
require "json"
require "securerandom"
require "sinatra"

set :bind, "127.0.0.1"
set :port, ENV.fetch("PORT", "4567").to_i
set :server, :puma
set :host_authorization, permitted_hosts: ["lvh.me", ".lvh.me"]

get "/" do
  content_type :html
  port = request.port == 80 || request.port == 443 ? "" : ":#{request.port}"
  <<~HTML
    <!doctype html>
    <html lang="en">
      <head>
        <meta charset="utf-8">
        <meta name="viewport" content="width=device-width, initial-scale=1">
        <title>Sinatra service</title>
        <link rel="stylesheet" href="/assets/app.css">
      </head>
      <body>
        <main>
          <h1>Hello from Sinatra</h1>
          <p class="muted">Served by dboss on port #{settings.port}.</p>

          <h2>dboss events</h2>
          <p>Every request to <a href="/shop">/shop</a> writes page_view, checkout_started and checkout_completed events to <code>log/shop.json.log</code>. dboss stores them as Parquet: try <code>dboss events sinatra</code>, <code>dboss events sinatra --facets plan</code> or the console's Events tab.</p>

          <h2>dboss pages</h2>
          <p>dboss answers some requests itself. This app ships one <code>public/error_pages/template.html</code>, so every page dboss shows for it uses the app's own look.</p>
          <ul class="tries">
            <li>
              <strong><a href="/boom">/boom</a></strong>
              <p>The app answers 500. dboss replaces the body with the error page from the template for a browser; <code>curl</code> still gets the plain <code>boom</code>.</p>
            </li>
            <li>
              <strong><a href="#{request.scheme}://nope.lvh.me#{port}/">nope.lvh.me</a></strong>
              <p>A host no app owns. dboss answers with the host's 404 page, here the built-in one.</p>
            </li>
            <li>
              <strong><code>dboss maintenance sinatra on</code></strong>
              <p>Every request gets the maintenance page from the same template while the app keeps running; <code>off</code> brings it back.</p>
            </li>
            <li>
              <strong><code>dboss pages sinatra</code></strong>
              <p>Lists which file serves each page. <code>dboss pages dump sinatra error</code> writes <code>error.html</code> next to the template to override just that one.</p>
            </li>
          </ul>
        </main>
      </body>
    </html>
  HTML
end

# Analytics events: one JSON object per line in log/<namespace>.json.log. dboss moves event,
# user_id, anon_id, tenant_id, request_id and value out of data into columns and stores the rows
# as Parquet; see them with `dboss events sinatra` or in the console's Events tab.
EVENTS_LOG = File.join(__dir__, "log", "shop.json.log")
FileUtils.mkdir_p(File.dirname(EVENTS_LOG))

helpers do
  def track(event, msg: nil, tags: [], **data)
    line = JSON.generate(msg: msg, tags: tags, data: data.merge(event: event, request_id: request.env["HTTP_X_REQUEST_ID"]))
    File.open(EVENTS_LOG, "a") { |file| file.puts(line) }
  end
end

before do
  @visitor = request.cookies["visitor"] || SecureRandom.hex(4)
  response.set_cookie("visitor", value: @visitor, path: "/") unless request.cookies["visitor"]
end

get "/shop" do
  plan = %w[free pro team].sample
  track("page_view", tags: ["page:pricing", "plan:#{plan}"], anon_id: @visitor)
  track("checkout_started", tags: ["plan:#{plan}"], anon_id: @visitor, user_id: "u_#{@visitor}") if rand < 0.6
  if rand < 0.4
    track("checkout_completed", msg: "Paid #{plan}", tags: ["plan:#{plan}"], anon_id: @visitor, user_id: "u_#{@visitor}", value: { "free" => 0, "pro" => 29, "team" => 99 }[plan], items: rand(1..3))
  end
  content_type :json
  JSON.generate(plan: plan, visitor: @visitor)
end

get "/up" do
  content_type :json
  JSON.generate(service: "sinatra", status: "ok")
end

# Always fails, to show the error page from public/error_pages and the error-rate alert.
get "/boom" do
  halt 500, "boom"
end
