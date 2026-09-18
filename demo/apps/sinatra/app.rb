# frozen_string_literal: true

require "json"
require "sinatra"

set :bind, "127.0.0.1"
set :port, ENV.fetch("PORT", "4567").to_i
set :server, :puma
set :host_authorization, permitted_hosts: ["lvh.me", ".lvh.me"]

get "/" do
  content_type :html
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
          <p>Served by app-boss on port #{settings.port}.</p>
        </main>
      </body>
    </html>
  HTML
end

get "/up" do
  content_type :json
  JSON.generate(service: "sinatra", status: "ok")
end
