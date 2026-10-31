package main

import "strings"

// adminHead renders the shared shell: nav tabs + CSS. active tab theo page.
// adminHeadTabs renders just the nav tab bar.
func adminHeadTabs(active string, chromes bool) string {
	type tab struct{ id, label string }
	tabs := []tab{{"dashboard", "stats"}, {"models", "models"}, {"config", "config"}}
	if chromes {
		tabs = append(tabs, tab{"chromes", "chrome captcha"})
	}
	b := strings.Builder{}
	for _, t := range tabs {
		cls := "tab"
		if t.id == active {
			cls += " on"
		}
		b.WriteString(`<a class="` + cls + `" href="?page=` + t.id + `">` + t.label + `</a>`)
	}
	return b.String()
}

func adminHead(active string, chromes bool) string {
	return `<!doctype html><meta charset="utf-8"><title>Gateway admin</title>
<meta name=viewport content="width=device-width,initial-scale=1">
<style>
:root{--bg:#0f1115;--card:#171a21;--line:#242833;--fg:#e6e8ee;--dim:#8b93a7;--on:#3fb27f;--bad:#e06c6c;--warn:#e0a84c}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 ui-sans-serif,system-ui,"Segoe UI",sans-serif}
header{position:sticky;top:0;z-index:2;padding:1rem 1.25rem;border-bottom:1px solid var(--line);
  background:rgba(15,17,21,.93);backdrop-filter:blur(8px)}
.wrap,main{max-width:60rem;margin:0 auto}
main{padding:1rem 1.25rem 4rem}
h1{margin:0;font-size:1.15rem}
.sub{color:var(--dim);font-size:.82rem;margin-top:.15rem}
.bar{display:flex;gap:.5rem;margin-top:.75rem;flex-wrap:wrap}
input[type=search]{flex:1;min-width:12rem;background:var(--card);border:1px solid var(--line);
  color:var(--fg);border-radius:8px;padding:.45rem .7rem;font:inherit}
button{background:var(--card);border:1px solid var(--line);color:var(--fg);border-radius:8px;
  padding:.45rem .8rem;font:inherit;cursor:pointer}
button:hover{background:#1d212b;border-color:#39405a}
h2{color:var(--dim);font-size:.72rem;text-transform:uppercase;letter-spacing:.09em;
  margin:1.4rem 0 .4rem;font-weight:600}
.row{display:flex;align-items:center;gap:.75rem;padding:.5rem .75rem;margin-bottom:.35rem;
  border:1px solid var(--line);border-radius:10px;background:var(--card);cursor:pointer}
.row:hover{border-color:#333a4a}
.row.hide{display:none}
.name{flex:1;display:flex;align-items:center;gap:.5rem}
.id{color:var(--dim);font-size:.75rem;font-family:ui-monospace,Consolas,monospace}
.badge{color:var(--bad);border:1px solid var(--bad);border-radius:999px;font-size:.66rem;
  padding:0 .4rem;line-height:1.4}
.badge.ok{color:var(--on);border-color:var(--on)}
.badge.warn{color:var(--warn);border-color:var(--warn)}
.sw{appearance:none;flex:none;width:40px;height:22px;border:0;border-radius:999px;background:#2b3040;
  position:relative;cursor:pointer;transition:background .15s}
.sw:checked{background:var(--on)}
.sw::after{content:"";position:absolute;top:3px;left:3px;width:16px;height:16px;border-radius:50%;
  background:#fff;transition:transform .15s}
.sw:checked::after{transform:translateX(18px)}
.row:has(.sw:not(:checked)) .name{color:var(--dim);text-decoration:line-through}
.del-btn{flex:none;background:none;border:1px solid transparent;color:var(--dim);font-size:1.1rem;
  padding:0 .4rem;border-radius:6px;line-height:1;cursor:pointer;opacity:0;transition:opacity .15s,color .15s}
.row:hover .del-btn{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad)}
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));gap:.5rem;
  padding:.75rem;border:1px solid var(--line);border-radius:10px;background:var(--card);margin-bottom:.6rem}
.card input{background:var(--bg);border:1px solid var(--line);color:var(--fg);border-radius:8px;
  padding:.45rem .6rem;font:inherit;min-width:0}
.card button{white-space:nowrap}
.settings-card{display:block}
.settings-card select,.settings-card input:not([type=checkbox]){display:block;width:min(100%,34rem);box-sizing:border-box;margin:.35rem 0 .8rem;
  background:var(--bg);border:1px solid var(--line);color:var(--fg);border-radius:8px;padding:.45rem .6rem;font:inherit}
.settings-card label{color:var(--dim);font-size:.82rem}
.cycle-list{max-height:18rem;overflow:auto;border:1px solid var(--line);border-radius:8px;margin:.35rem 0 .8rem}
.cycle-option{display:flex;align-items:center;justify-content:space-between;gap:.5rem;padding:.25rem .5rem;border-bottom:1px solid var(--line)}
.cycle-option:last-child{border-bottom:0}
.cycle-option label{display:flex;align-items:center;gap:.5rem;overflow-wrap:anywhere;color:var(--fg)}
.cycle-option input[type=checkbox]{flex:none;width:1rem;height:1rem;margin:0;padding:0}
.cycle-option button{padding:.1rem .4rem}

#toast{position:fixed;right:1rem;bottom:1rem;padding:.5rem .8rem;border-radius:8px;
  border:1px solid var(--line);background:var(--card);opacity:0;transition:opacity .2s}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(8.5rem,1fr));gap:.5rem;margin-bottom:.6rem}
.tile{border:1px solid var(--line);border-radius:10px;background:var(--card);padding:.7rem .8rem}
.tile .big{font-size:1.35rem;font-weight:600;line-height:1.2}
.tile .lbl{color:var(--dim);font-size:.72rem;margin-bottom:.1rem}
.chartbox{border:1px solid var(--line);border-radius:10px;background:var(--card);padding:.6rem .8rem .5rem;margin-bottom:.6rem}
.chartrow{display:grid;grid-template-columns:repeat(auto-fit,minmax(26rem,1fr));gap:.6rem;margin-bottom:.6rem}
.chartrow .chartbox{margin-bottom:0}
.note{color:var(--dim);font-size:.72rem;margin:.2rem 0 .5rem}
.pb{border:1px solid;border-radius:999px;font-size:.66rem;padding:0 .4rem;line-height:1.4;white-space:nowrap;margin-right:.4rem}
#frames{display:flex;gap:.3rem;flex-wrap:wrap;margin-bottom:.5rem}
#frames button{padding:.2rem .55rem;font-size:.78rem}
.fp.on{background:#223044;border-color:#39405a}
.logwrap{max-height:22rem;overflow:auto;border:1px solid var(--line);border-radius:10px}
.chhead{display:flex;gap:1rem;margin-bottom:.3rem}
.lg{color:var(--dim);font-size:.72rem;display:inline-flex;align-items:center;gap:.3rem}
.chwrap{position:relative}
.ax{fill:var(--dim);font-size:9px;font-family:ui-monospace,Consolas,monospace}
.tip{position:absolute;pointer-events:none;background:var(--card);border:1px solid var(--line);
  border-radius:8px;padding:.45rem .6rem;min-width:110px;font-size:.72rem;box-shadow:0 4px 14px rgba(0,0,0,.4);z-index:3}
.tip.hidden{display:none}
.tip-h{color:var(--dim);margin-bottom:.25rem;font-family:ui-monospace,Consolas,monospace}
.tip-r{display:flex;justify-content:space-between;gap:1rem;line-height:1.5}
.tip-k{color:var(--dim);display:inline-flex;align-items:center;gap:.35rem}
.tip-sw{width:10px;height:2px;border-radius:1px;display:inline-block}
.tip-r b{font-weight:600}
table.stats{width:100%;border-collapse:collapse;font-size:.8rem;overflow:hidden;border-radius:10px}
table.stats th,table.stats td{padding:.4rem .6rem;border-bottom:1px solid var(--line);text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);background:var(--card);position:sticky;top:0}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" ↑"}
table.stats thead th.on:not(.asc)::after{content:" ↓"}
table.stats td.nm{text-align:left;font-family:ui-monospace,Consolas,monospace;font-size:.75rem}
table.stats tbody tr:hover{background:#1d212b}
table.stats tfoot th{color:var(--dim);background:var(--card)}
#toast.show{opacity:1}
#deleted-section{margin-top:1.5rem;border:1px solid var(--line);border-radius:10px;background:var(--card);overflow:hidden}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.65rem .85rem;cursor:pointer;user-select:none}
#deleted-header:hover{background:#1d212b}
#deleted-header .cnt{color:var(--warn);font-size:.78rem}
#deleted-header .arrow{color:var(--dim);font-size:.7rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.4rem .85rem;border-bottom:1px solid var(--line)}
.del-row:last-child{border-bottom:0}
.del-row .id{flex:1}
.del-row button{border-color:var(--on);color:var(--on);padding:.2rem .55rem;font-size:.8rem}
/* NAV TABS */
	.tab-bar{display:flex;gap:.4rem;padding-top:.5rem;border-top:1px solid var(--line);margin-top:.75rem}
	.tab{color:var(--dim);font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;
	  padding:.3rem .6rem;border-radius:6px;text-decoration:none}
	.tab:hover{color:var(--fg);background:#1d212b}
	.tab.on{color:var(--fg);background:#223044}
	/* hide search on non-models pages */
	#q:not([data-page=models]){display:none}
	</style>
<header><div class=wrap>
  <h1>Gateway admin</h1>
  <div class=tab-bar>` + adminHeadTabs(active, chromes) + `</div>
  <div class=sub id=counts></div>
  <div class=bar>
    <input type=search id=q placeholder="search models…" autofocus data-page="models">
    <button id=all>Enable all</button>
    <button id=none>Disable all</button>
    <button id=retry>Retry failed models</button>
  </div>
</div></header>
`
}

// providerForm is the "add an endpoint" card rendered above the model list.
const providerForm = `<form id=addform class=card>
  <input name=name placeholder="name (e.g. openrouter)" required>
  <input name=base_url placeholder="base URL or full endpoint (https://opencode.ai/zen/v1/responses)" required>
  <input name=api_key placeholder="api key" type=password>
  <input name=model placeholder="their model id (e.g. x-ai/grok-4)" required>
  <label style="display:flex;gap:.4rem;align-items:center;font-size:.8rem;color:var(--dim)"><input type=checkbox name=responses_only>OpenAI Responses API (/v1/responses)</label>
  <label style="display:flex;gap:.4rem;align-items:center;font-size:.8rem;color:var(--dim)"><input type=checkbox name=messages_only>Anthropic Messages API (/v1/messages)</label>
  <button type=submit>Add endpoint</button>
</form>`
