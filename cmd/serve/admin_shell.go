package main

import "strings"

// adminTitle is the name shown next to the brand for each tab. It is longer
// than the tab label where the tab alone would be ambiguous.
var adminTitle = map[string]string{
	"dashboard": "Stats",
	"models":    "Models",
	"config":    "Config",
	"chromes":   "Captcha Chrome",
}

// adminHeadTabs renders the nav bar, marking active with .on. Labels are kept
// to one word each so the bar renders as a single segmented control.
func adminHeadTabs(active string) string {
	type tab struct{ id, label string }
	tabs := []tab{{"dashboard", "Stats"}, {"models", "Models"}, {"config", "Config"}, {"chromes", "Chrome"}}
	b := strings.Builder{}
	for _, t := range tabs {
		cls, cur := "tab", ""
		if t.id == active {
			cls, cur = "tab on", ` aria-current="page"`
		}
		b.WriteString(`<a class="` + cls + `" href="?page=` + t.id + `"` + cur + `>` + t.label + `</a>`)
	}
	return b.String()
}

// adminHeadSub renders the line under the tabs: the models control bar on the
// models page, a static subtitle everywhere else. Both are emitted server-side
// per page rather than hidden at runtime, so no page briefly shows controls or
// counts it does not own.
func adminHeadSub(active string) string {
	if active == "models" {
		return modelsBarHTML
	}
	return pageSubHTML[active]
}

const modelsBarHTML = `<div class=bar>
    <input type=search id=q placeholder="Filter models by name or id…" autofocus>
    <button id=all>Enable all</button>
    <button id=none>Disable all</button>
    <button id=retry>Retry failed</button>
  </div>
  <div class=sub id=counts></div>`

// pageSubHTML is the static subtitle per non-models tab.
var pageSubHTML = map[string]string{
	"dashboard": `<div class=sub>Usage, latency and request history for every upstream API.</div>`,
	"config":    `<div class=sub>Auto model, model cycle, web search.</div>`,
	"chromes":   `<div class=sub>Captcha Chrome processes — start on demand, kill to free RAM.</div>`,
}

// adminHead renders the shared page shell: the stylesheet, the brand line, the
// tab bar and the page subtitle. adminHTML appends the page body to it.
func adminHead(active string) string {
	title := adminTitle[active]
	if title == "" {
		title = "Models"
	}
	return `<!doctype html><meta charset="utf-8"><title>` + title + ` · Gateway admin</title>
<meta name=viewport content="width=device-width,initial-scale=1">
<style>
:root{
  /* Neutrals carry a faint blue-green cast drawn from the live-state accent,
     so the chrome feels like one material instead of grey-plus-a-green-button. */
  --bg:#0b0e10;--bg-2:#0d1113;--panel:#12171a;--panel-2:#161d21;
  --line:#1e262b;--line-2:#2a353b;
  --fg:#e4e9ec;--dim:#8797a0;--dim-2:#62737c;
  --on:#3fb27f;--bad:#e06c6c;--warn:#e0a84c;--accent:#5b8def;
  --r-sm:6px;--r:8px;--r-lg:10px;
  /* Mono earns its place: every number here is compared across rows. */
  --mono:ui-monospace,"SF Mono","Cascadia Mono",Consolas,monospace;
}
*{box-sizing:border-box}
/* Tells the UA to render scrollbars and form widgets dark; without it the
   page is dark but every native control stays light-on-light. */
:root{color-scheme:dark}
html{-webkit-text-size-adjust:100%}
@media (prefers-reduced-motion:reduce){
  *,*::before,*::after{animation-duration:.01ms!important;animation-iteration-count:1!important;
    transition-duration:.01ms!important;scroll-behavior:auto!important}
}
body{margin:0;background:var(--bg-2);color:var(--fg);
  font:14px/1.55 ui-sans-serif,system-ui,"Segoe UI",Roboto,sans-serif;
  -webkit-font-smoothing:antialiased}
::selection{background:rgba(91,141,239,.35)}

/* ---- shell ---- */
header{position:sticky;top:0;z-index:20;padding:.9rem 0 .75rem;
  background:rgba(14,17,22,.86);backdrop-filter:blur(12px) saturate(1.2);
  border-bottom:1px solid var(--line)}
.wrap,main{max-width:64rem;margin:0 auto;padding:0 1.5rem}
main{padding:1.5rem 1.5rem 5rem}
/* The page you are on is the h1, not a floating label on the right: no section
   heading on the page needs to repeat it back. */
.brand{display:flex;align-items:baseline;gap:.5rem;margin-bottom:.7rem}
h1{margin:0;font-size:1.02rem;font-weight:620;letter-spacing:-.015em}
.brand .product{color:var(--dim-2);font-size:.78rem;font-weight:450}
/* No glyph on the h1: it is the page name, and the tab bar directly below
   already says which page this is. */
h1::before{content:none}
/* The page name is repeated by the section heading right below it on the
   models page; the brand line carries the product, the h1 carries the page. */
main>h2:first-child{margin-top:.2rem}

/* ---- nav tabs: segmented control ---- */
.tab-bar{display:inline-flex;gap:2px;padding:3px;background:var(--panel);
  border:1px solid var(--line);border-radius:var(--r)}
.tab{color:var(--dim);font-size:.8rem;font-weight:550;padding:.32rem .85rem;
  border-radius:7px;text-decoration:none;transition:background .12s,color .12s}
.tab:hover{color:var(--fg);background:var(--panel-2)}
.tab.on{color:var(--fg);background:var(--line-2);
  box-shadow:inset 0 0 0 1px rgba(255,255,255,.05)}
.tab:focus-visible{outline:2px solid var(--accent);outline-offset:1px}

/* ---- page header row ---- */
.sub{color:var(--dim);font-size:.8rem;margin-top:.7rem}
.bar{display:flex;gap:.5rem;margin-top:.85rem;flex-wrap:wrap;align-items:center}

/* ---- controls ---- */
input[type=search],.card input,.card select,.form-grid input,.form-grid select,
.settings-card select,.settings-card input:not([type=checkbox]),.mm-row input,.hdr-row input{
  background:var(--bg);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.45rem .65rem;font:inherit;
  transition:border-color .12s,box-shadow .12s}
input[type=search]{flex:1;min-width:13rem}
input:focus,select:focus,button:focus-visible,.sw:focus-visible{
  outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(91,141,239,.18)}
input::placeholder{color:var(--dim-2)}
button{background:var(--panel);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.42rem .8rem;font:inherit;font-size:.82rem;
  font-weight:550;cursor:pointer;transition:background .12s,border-color .12s}
button:hover{background:var(--panel-2);border-color:var(--line-2)}
button:disabled{opacity:.45;cursor:not-allowed}
#all,#none,#retry{background:transparent}

/* ---- section headings ---- */
/* Sentence-weight headings, not tracked-out all-caps labels: the page already
   has a title, and an eyebrow on every section is chrome that says nothing. */
h2{color:var(--fg);font-size:.88rem;letter-spacing:-.01em;
  margin:1.9rem 0 .6rem;font-weight:620;display:flex;align-items:center;gap:.7rem}
h2::after{content:"";flex:1;height:1px;background:var(--line)}
h3{letter-spacing:-.01em}

/* ---- rows ---- */
.row{display:flex;align-items:center;gap:.75rem;padding:.55rem .8rem;
  margin-bottom:.3rem;border:1px solid var(--line);border-radius:var(--r);
  background:var(--panel);cursor:pointer;transition:border-color .12s,background .12s}
.row:hover{border-color:var(--line-2);background:var(--panel-2)}
.row.hide{display:none}
/* .row is shared chrome: model toggles, provider cards, captcha status. Only
   model-toggle rows carry data-id + .sw; every models-only script hook below
   keys on that pair so other pages are left alone. */
.row.static{cursor:default}
.row.static:hover{border-color:var(--line);background:var(--panel)}
.name{flex:1;display:flex;align-items:center;gap:.5rem;min-width:0}
.id{color:var(--dim);font-size:.74rem;font-family:var(--mono)}

/* ---- badges ---- */
/* Status is carried by a word plus a shape, not by hue alone: the previous
   pill distinguished running from busy with color only, which is unreadable
   for a red-green colorblind operator. The dot is a second channel. */
.badge{color:var(--bad);font-size:.68rem;font-weight:550;line-height:1.5;
  padding:0 .1rem;white-space:nowrap;display:inline-flex;align-items:center;gap:.34rem}
.badge::before{content:"";width:6px;height:6px;flex:none;border-radius:50%;
  background:currentColor}
.badge.sq::before{border-radius:1px}
.badge.ok{color:var(--on)}
.badge.warn{color:var(--warn)}
.badge.mute{color:var(--dim-2)}
/* Provider tags on a model row read as data, not as state. */
.badge.pb{color:var(--on);opacity:.85;font-family:var(--mono);font-size:.66rem;
  border:1px solid var(--line-2);border-radius:4px;padding:0 .3rem}
.badge.pb::before{display:none}

/* ---- switch ---- */
.sw{appearance:none;flex:none;width:38px;height:21px;border:0;border-radius:999px;
  background:#2b3040;position:relative;cursor:pointer;transition:background .15s;
  margin:0;padding:0}
.sw:checked{background:var(--on)}
.sw::after{content:"";position:absolute;top:3px;left:3px;width:15px;height:15px;
  border-radius:50%;background:#fff;transition:transform .15s;
  box-shadow:0 1px 2px rgba(0,0,0,.4)}
.sw:checked::after{transform:translateX(17px)}
.row:has(.sw:not(:checked)) .name{color:var(--dim);text-decoration:line-through;
  text-decoration-color:var(--dim-2)}
.del-btn{flex:none;background:none;border:1px solid transparent;color:var(--dim);
  font-size:1.1rem;padding:0 .4rem;border-radius:6px;line-height:1;cursor:pointer;
  opacity:0;transition:opacity .15s,color .15s}
.row:hover .del-btn{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad)}

/* ---- cards ---- */
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));
  gap:.5rem;padding:.85rem;border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);margin-bottom:.6rem}
.card button{white-space:nowrap}
.settings-card{display:block;padding:1.1rem 1.15rem}
.settings-card p{color:var(--dim);font-size:.82rem;margin:.1rem 0 1rem;max-width:52rem}
.settings-card code{font-family:var(--mono);font-size:.78rem;color:var(--fg);
  background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:.05rem .3rem}
.settings-card select,.settings-card input:not([type=checkbox]){display:block;
  width:min(100%,32rem);margin:.35rem 0 .9rem}
.settings-card label{color:var(--dim);font-size:.78rem;font-weight:550}
.prov-card{display:block;padding:.8rem .95rem;border:1px solid var(--line);
  border-radius:var(--r-lg);background:var(--panel);margin-bottom:.5rem;
  transition:border-color .12s}
.prov-card:hover{border-color:var(--line-2)}
.prov-head{display:flex;align-items:center;gap:.6rem;flex-wrap:wrap}
.prov-head strong{letter-spacing:-.01em}
.prov-meta{color:var(--dim-2);font-size:.76rem;font-family:var(--mono);
  margin-left:auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  max-width:40%}
.prov-aliases{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.55rem}
.prov-actions{display:flex;gap:.4rem;align-items:center;flex-wrap:wrap;margin-top:.65rem}
.dim{color:var(--dim)}

/* ---- forms ---- */
.form-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(13rem,1fr));
  gap:.5rem .75rem;margin-bottom:.5rem}
.form-grid label,.form-grid input,.form-grid select{display:block;width:100%}
.form-grid label{color:var(--dim);font-size:.76rem;font-weight:550;margin-bottom:.2rem}
.form-grid input,.form-grid select{margin-bottom:.3rem}
.mm-row,.hdr-row{display:flex;gap:.4rem;align-items:center;margin-bottom:.35rem}
.mm-row input,.hdr-row input{font-size:.82rem;min-width:0;padding:.35rem .5rem}
.mm-row input.mm-alias{flex:1.2}
.mm-row input.mm-upstream{flex:1.5}
.mm-row input.mm-fallbacks{flex:1.3}
.mm-row label{display:flex;align-items:center;gap:.25rem;color:var(--dim);
  font-size:.74rem;white-space:nowrap;margin:0 .2rem}
.hdr-row input.hdr-name{flex:1}
.hdr-row input.hdr-val{flex:1.5}
.del-row-btn{background:none;border:none;color:var(--dim);font-size:1.2rem;
  cursor:pointer;padding:0 .3rem;line-height:1}
.del-row-btn:hover{color:var(--bad);background:none}
.btn-sm{padding:.28rem .6rem;font-size:.78rem}
.btn-del{color:var(--bad);border-color:color-mix(in srgb,var(--bad) 45%,var(--line))}
.btn-del:hover{background:rgba(224,108,108,.12);border-color:var(--bad)}
.test-res{font-size:.8rem;display:inline-flex;align-items:center;gap:.4rem}

/* ---- cycle list ---- */
.cycle-list{max-height:18rem;overflow:auto;border:1px solid var(--line);
  border-radius:var(--r-sm);margin:.35rem 0 .9rem;background:var(--bg)}
.cycle-option{display:flex;align-items:center;justify-content:space-between;
  gap:.5rem;padding:.32rem .6rem;border-bottom:1px solid var(--line)}
.cycle-option:last-child{border-bottom:0}
.cycle-option label{display:flex;align-items:center;gap:.5rem;
  overflow-wrap:anywhere;color:var(--fg)}
.cycle-option input[type=checkbox]{flex:none;width:1rem;height:1rem;margin:0;padding:0;
  accent-color:var(--accent)}
.cycle-option button{padding:.1rem .4rem}

/* ---- toast ---- */
#toast{position:fixed;right:1.25rem;bottom:1.25rem;padding:.6rem .9rem;
  border-radius:var(--r);border:1px solid var(--line-2);background:var(--panel-2);
  box-shadow:0 10px 30px rgba(0,0,0,.5);opacity:0;transform:translateY(6px);
  transition:opacity .18s,transform .18s;pointer-events:none;font-size:.82rem}
#toast.show{opacity:1;transform:none}

/* ---- dashboard ---- */
/* A reading board, not eight identical cards: one continuous sheet divided by
   hairlines, so the numbers compare down a column instead of floating apart. */
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5rem,1fr));
  gap:1px;margin-bottom:.9rem;background:var(--line);
  border:1px solid var(--line);border-radius:var(--r-lg);overflow:hidden}
.tile{background:var(--panel);padding:.75rem .9rem .7rem}
.tile .lbl{color:var(--dim);font-size:.74rem;margin-bottom:.1rem;font-weight:500}
.tile .big{font-size:1.5rem;font-weight:600;line-height:1.2;letter-spacing:-.03em;
  font-family:var(--mono);font-variant-numeric:tabular-nums}
.tile svg{margin-top:.45rem}
.chartbox{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  padding:.7rem .9rem .55rem;margin-bottom:.6rem}
.chartrow{display:grid;grid-template-columns:repeat(auto-fit,minmax(24rem,1fr));
  gap:.6rem;margin-bottom:.6rem}
.chartrow .chartbox{margin-bottom:0}
.note{color:var(--dim-2);font-size:.74rem;margin:.25rem 0 .6rem;max-width:60rem}
#frames{display:flex;gap:.3rem;flex-wrap:wrap;margin-bottom:.6rem}
#frames button{padding:.2rem .55rem;font-size:.76rem;font-weight:500}
.fp.on{background:var(--line-2);border-color:var(--line-2);color:var(--fg)}
.logwrap{max-height:22rem;overflow:auto;border:1px solid var(--line);
  border-radius:var(--r-lg);background:var(--panel)}
.chhead{display:flex;gap:1rem;margin-bottom:.4rem;flex-wrap:wrap}
.lg{color:var(--dim);font-size:.72rem;display:inline-flex;align-items:center;gap:.3rem}
.chwrap{position:relative}
.ax{fill:var(--dim-2);font-size:9px;font-family:var(--mono)}
.tip{position:absolute;pointer-events:none;background:var(--panel-2);
  border:1px solid var(--line-2);border-radius:var(--r-sm);padding:.5rem .65rem;
  min-width:120px;font-size:.72rem;box-shadow:0 8px 24px rgba(0,0,0,.55);z-index:5}
.tip.hidden{display:none}
.tip-h{color:var(--dim);margin-bottom:.3rem;font-family:var(--mono);font-size:.7rem}
.tip-r{display:flex;justify-content:space-between;gap:1rem;line-height:1.6}
.tip-k{color:var(--dim);display:inline-flex;align-items:center;gap:.35rem}
.tip-sw{width:10px;height:2px;border-radius:1px;display:inline-block}
.tip-r b{font-weight:600;font-variant-numeric:tabular-nums}
table.stats{width:100%;border-collapse:collapse;font-size:.79rem;
  font-variant-numeric:tabular-nums}
table.stats th,table.stats td{padding:.45rem .65rem;border-bottom:1px solid var(--line);
  text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);
  background:var(--panel);position:sticky;top:0;font-weight:550;font-size:.74rem}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" ↑"}
table.stats thead th.on:not(.asc)::after{content:" ↓"}
table.stats td.nm{text-align:left;font-family:var(--mono);font-size:.74rem}
table.stats tbody tr:hover{background:var(--panel-2)}
table.stats tfoot th{color:var(--dim);background:var(--panel)}

/* ---- deleted models ---- */
#deleted-section{margin-top:1.75rem;border:1px solid var(--line);
  border-radius:var(--r-lg);background:var(--panel);overflow:hidden}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.7rem .9rem;
  cursor:pointer;user-select:none;font-size:.82rem;font-weight:550}
#deleted-header:hover{background:var(--panel-2)}
#deleted-header .cnt{color:var(--warn);font-size:.76rem;font-weight:600}
#deleted-header .arrow{color:var(--dim);font-size:.7rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.42rem .9rem;
  border-bottom:1px solid var(--line)}
.del-row:last-child{border-bottom:0}
.del-row .id{flex:1}
.del-row button{border-color:color-mix(in srgb,var(--on) 45%,var(--line));
  color:var(--on);padding:.2rem .55rem;font-size:.78rem}
.del-row button:hover{background:rgba(63,178,127,.12);border-color:var(--on)}

/* ---- narrow screens ---- */
@media (max-width:640px){
  .wrap,main{padding-left:1rem;padding-right:1rem}
  .brand .product{display:none}
  .tab-bar{display:flex;width:100%}
  .tab{flex:1;text-align:center}
  .prov-meta{display:none}
  .chartrow{grid-template-columns:1fr}
}
</style>
<header><div class=wrap>
  <div class=brand><h1>` + title + `</h1><span class=product>Gateway</span></div>
  <nav class=tab-bar aria-label="Admin sections">` + adminHeadTabs(active) + `</nav>
  ` + adminHeadSub(active) + `
</div></header>
`
}
