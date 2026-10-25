package api

import (
	"html/template"
	"net/http"
	"strings"
)

// The start page at "/". It needs no token: it only shows public information about
// this deployment, plus the development tokens that are already in principals.yaml.

type indexPrincipal struct {
	ID, Name, Roles, Token string
}

type indexTool struct {
	Name, MCPName, Summary, Risk, Autonomy string
}

type indexData struct {
	Version, Pack, DBStatus string
	Healthy                 bool
	Tools                   []indexTool
	ToolsTotal              int
	Principals              []indexPrincipal
}

var indexTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tamarind LOS</title>
<style>
  :root { --bg:#fbfaf7; --fg:#1d1b18; --muted:#6b665e; --line:#e4e0d8; --card:#fff; --accent:#8a4b1f; --ok:#2f7d4f; --bad:#b3261e; }
  @media (prefers-color-scheme: dark) { :root { --bg:#161513; --fg:#ece8e1; --muted:#a39d93; --line:#34312c; --card:#1f1d1a; --accent:#e0965c; --ok:#6cc08b; --bad:#f2827a; } }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.55 system-ui, -apple-system, "Segoe UI", sans-serif; }
  main { max-width: 960px; margin: 0 auto; padding: 32px 16px 64px; }
  h1 { margin:0 0 4px; font-size: 28px; }
  h2 { margin: 36px 0 10px; font-size: 18px; }
  p { margin: 6px 0; }
  .sub { color: var(--muted); }
  .badge { display:inline-block; padding:2px 10px; border-radius:999px; font-size:13px; border:1px solid var(--line); }
  .ok { color: var(--ok); } .bad { color: var(--bad); }
  .warn { border-left: 3px solid var(--accent); background: var(--card); padding: 10px 14px; margin: 18px 0; }
  table { width:100%; border-collapse: collapse; background: var(--card); border:1px solid var(--line); }
  th, td { text-align:left; padding:8px 10px; border-bottom:1px solid var(--line); vertical-align: top; }
  th { font-size: 13px; color: var(--muted); font-weight: 600; }
  code, pre { font: 13px/1.5 ui-monospace, SFMono-Regular, Consolas, monospace; }
  pre { background: var(--card); border:1px solid var(--line); padding: 12px; overflow-x: auto; }
  a { color: var(--accent); }
  .scroll { overflow-x: auto; }
</style>
</head>
<body>
<main>
  <h1>Tamarind LOS</h1>
  <p class="sub">Open-source, agent-native loan origination system — core tool gateway</p>
  <p>
    <span class="badge">version {{.Version}}</span>
    <span class="badge">policy pack {{.Pack}}</span>
    <span class="badge {{if .Healthy}}ok{{else}}bad{{end}}">database {{.DBStatus}}</span>
  </p>

  <div class="warn"><strong>Development deployment.</strong> Authentication uses the static test tokens below. Do not use this setup with real data.</div>

  <h2>Endpoints</h2>
  <div class="scroll"><table>
    <tr><th>Endpoint</th><th>Purpose</th><th>Token</th></tr>
    <tr><td><a href="/healthz"><code>GET /healthz</code></a></td><td>Health and database check</td><td>No</td></tr>
    <tr><td><code>GET /v1/tools</code></td><td>Tool catalogue, with what is implemented and what the caller may use</td><td>Yes</td></tr>
    <tr><td><code>POST /v1/tools/{name}</code></td><td>Invoke a tool (REST)</td><td>Yes</td></tr>
    <tr><td><code>POST /mcp</code></td><td>MCP endpoint for AI agents (Streamable HTTP, JSON responses)</td><td>Yes</td></tr>
    <tr><td><code>PUT /v1/uploads/{token}</code></td><td>Upload a document to a single-use link from <code>document.upload_url</code></td><td>No (the link is the credential)</td></tr>
    <tr><td><code>GET /v1/events?after=0&amp;wait=25</code></td><td>Ordered event feed with long polling; the built-in agents wake on it</td><td>Yes (<code>events:read</code>)</td></tr>
  </table></div>

  <h2>Implemented tools ({{len .Tools}} of {{.ToolsTotal}})</h2>
  <div class="scroll"><table>
    <tr><th>Tool</th><th>MCP name</th><th>Summary</th><th>Risk</th><th>Agent autonomy</th></tr>
    {{range .Tools}}<tr><td><code>{{.Name}}</code></td><td><code>{{.MCPName}}</code></td><td>{{.Summary}}</td><td>{{.Risk}}</td><td>{{.Autonomy}}</td></tr>
    {{end}}
  </table></div>

  <h2>Development tokens</h2>
  <div class="scroll"><table>
    <tr><th>Principal</th><th>Who</th><th>Roles</th><th>Token</th></tr>
    {{range .Principals}}<tr><td><code>{{.ID}}</code></td><td>{{.Name}}</td><td>{{.Roles}}</td><td><code>{{.Token}}</code></td></tr>
    {{end}}
  </table></div>

  <h2>Try it from PowerShell</h2>
<pre>$h = @{ Authorization = "Bearer dev-agent-intake" }
$body = @{
  input = @{ product_code = "SME-WC"; requested_amount_minor = 3000000; currency = "USD"; requested_term_months = 12 }
  idempotency_key = [guid]::NewGuid().ToString()
  reason = "new enquiry"
} | ConvertTo-Json -Depth 5
Invoke-RestMethod http://localhost:8080/v1/tools/application.create -Method Post -Headers $h -Body $body -ContentType "application/json"</pre>

  <h2>Try it with curl</h2>
<pre>curl -s http://localhost:8080/v1/tools/application.create \
  -H "Authorization: Bearer dev-agent-intake" \
  -d '{"input": {"product_code": "SME-WC", "requested_amount_minor": 3000000, "currency": "USD", "requested_term_months": 12},
       "idempotency_key": "demo-1", "reason": "new enquiry"}'</pre>

  <h2>Connect an MCP client</h2>
  <p>Point any MCP client that supports Streamable HTTP at <code>http://localhost:8080/mcp</code> with the header
  <code>Authorization: Bearer dev-agent-intake</code>. Put call metadata in an <code>_envelope</code> argument, for example
  <code>{"reason": "new enquiry", "idempotency_key": "mcp-1"}</code>. Agents must always give a reason.</p>

  <h2>Learn more</h2>
  <p><a href="https://github.com/TamarindTech/TamarindLOS">github.com/TamarindTech/TamarindLOS</a> — concept paper, reference architecture, specifications and decisions.</p>
</main>
</body>
</html>
`))

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	d := indexData{Version: Version, Pack: s.G.Pack.Name + "@" + s.G.Pack.Version, DBStatus: "connected", Healthy: true,
		ToolsTotal: len(s.G.Catalogue.Names)}
	if err := s.G.DB.Ping(r.Context()); err != nil {
		d.DBStatus, d.Healthy = "unavailable", false
	}
	for _, n := range s.G.Catalogue.Names {
		if !s.G.Implemented(n) {
			continue
		}
		t := s.G.Catalogue.Tools[n]
		d.Tools = append(d.Tools, indexTool{Name: n, MCPName: MCPName(n), Summary: t.Summary, Risk: t.RiskTier, Autonomy: t.DefaultAutonomy})
	}
	for _, p := range s.G.Registry.Principals {
		d.Principals = append(d.Principals, indexPrincipal{ID: p.ID, Name: p.Name, Roles: strings.Join(p.Roles, ", "), Token: p.Token})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = indexTemplate.Execute(w, d)
}
