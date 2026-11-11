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

// adminHeadSub renders the strip under the tabs: the models toolbar on the
// models page, a one-line scope note everywhere else. Both are emitted
// server-side per page rather than hidden at runtime, so no page briefly shows
// controls or counts it does not own.
func adminHeadSub(active string) string {
	if active == "models" {
		return modelsBarHTML
	}
	return pageSubHTML[active]
}

const modelsBarHTML = `<div class=tools>
    <input type=search id=q placeholder="Filter by name or model id" autofocus>
    <button id=all>Enable all</button>
    <button id=none>Disable all</button>
    <button id=retry>Retry failed</button>
    <span class=sub id=counts></span>
  </div>`

// pageSubHTML is the static scope note per non-models tab.
var pageSubHTML = map[string]string{
	"dashboard": `<div class=sub>Usage, latency and request history for every upstream API.</div>`,
	"config":    `<div class=sub>Auto model, model cycle, web search.</div>`,
	"chromes":   `<div class=sub>Captcha Chrome processes — start on demand, kill to free RAM.</div>`,
}

// adminHead renders the shared page shell: the stylesheet, the header strip
// (brand, tabs, page toolbar) and nothing else. adminHTML appends <main>.
//
// Stylesheet notes: the page is server-rendered with inline CSS and JS, so
// every rule here has to stand on its own — no CDN, no build step, no external
// font. Layout is built from two containers: .sheet (a bordered panel whose
// children are divided by hairlines, used for anything list-shaped) and plain
// sections for prose and charts.
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
     so the chrome reads as one material instead of grey-plus-a-green-button.
     The chart series colors in the stats script were validated against
     --panel; keep that value stable when touching this block. */
  --bg:#0b0e10;--bg-2:#0e1114;--panel:#131a1d;--panel-2:#182125;
  --line:#1f282d;--line-2:#2c373d;
  --fg:#e6ebee;--dim:#8b9aa3;--dim-2:#63747d;
  --on:#3fb27f;--bad:#e0706f;--warn:#dda24d;--accent:#5b8def;
  --r-sm:6px;--r:9px;--r-lg:11px;
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
a{color:var(--accent)}

/* ---- shell ---- */
header{position:sticky;top:0;z-index:20;
  background:rgba(14,17,20,.9);backdrop-filter:blur(12px) saturate(1.2);
  border-bottom:1px solid var(--line)}
.wrap,main{max-width:64rem;margin:0 auto;padding:0 1.4rem}
.head-row{display:flex;align-items:center;gap:1rem;flex-wrap:wrap;padding:.65rem 0 .6rem}
/* The page you are on is the h1; no section heading below repeats it back. */
.brand{display:flex;align-items:baseline;gap:.5rem;margin-right:auto}
h1{margin:0;font-size:1.16rem;font-weight:640;letter-spacing:-.02em}
.brand .product{color:var(--dim-2);font-size:.74rem;font-weight:450;letter-spacing:.01em}
/* The toolbar belongs to the header strip so it stays glued to the top on the
   one page that scrolls through hundreds of rows. */
.tools{display:flex;gap:.4rem;align-items:center;flex-wrap:wrap;
  padding:0 0 .65rem}
.tools .sub{margin:0 0 0 auto}
input[type=search]{flex:1;min-width:12rem;max-width:22rem}
.sub{color:var(--dim);font-size:.78rem;padding:0 0 .7rem}

/* ---- nav tabs: segmented control ---- */
.tab-bar{display:inline-flex;gap:2px;padding:3px;background:var(--panel);
  border:1px solid var(--line);border-radius:var(--r)}
.tab{color:var(--dim);font-size:.79rem;font-weight:550;padding:.3rem .8rem;
  border-radius:6px;text-decoration:none;transition:background .12s,color .12s}
.tab:hover{color:var(--fg);background:var(--panel-2)}
.tab.on{color:var(--fg);background:var(--line-2);
  box-shadow:inset 0 0 0 1px rgba(255,255,255,.05)}
.tab:focus-visible{outline:2px solid var(--accent);outline-offset:1px}

main{padding:1.4rem 1.4rem 5rem;display:flex;flex-direction:column;gap:.7rem}

/* ---- sheets: the one list container ---- */
/* A sheet is a bordered panel whose children are divided by hairlines. Rows
   keep their own look; only the container changes. Providers, workers, the
   model catalog and the request log all share it so the eye learns one rule. */
.sheet{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  overflow:hidden}
.sheet>.row,.sheet>.prov-card,.sheet>.del-row{border:0;border-radius:0;margin:0;
  border-bottom:1px solid var(--line)}
.sheet>:last-child{border-bottom:0}
.sheet-head{display:flex;align-items:center;gap:.7rem;flex-wrap:wrap;
  padding:.55rem .9rem;background:var(--panel-2);border-bottom:1px solid var(--line)}
.sheet-head h2{margin:0;font-size:.86rem}
.sheet-head h2::after{content:none}
.sheet-head .sub{padding:0;margin-left:auto}
.sheet-title{font-size:.86rem;font-weight:620;letter-spacing:-.01em}
.sheet-body{padding:.9rem}
.sheet-foot{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap;
  padding:.6rem .9rem;border-top:1px solid var(--line);background:var(--panel-2)}
/* A <details> used as a sheet: the summary is the sheet header. */
details.sheet>summary{list-style:none;cursor:pointer;user-select:none}
details.sheet>summary::-webkit-details-marker{display:none}
details.sheet>summary::after{content:"+";margin-left:auto;color:var(--dim);
  font-size:.95rem;line-height:1}
details.sheet[open]>summary::after{content:"–"}
details.sheet>summary:hover{background:var(--line)}
/* Tiles already carry their own sheet; nested inside one they only add a rule. */
.sheet .tiles{border:0;border-radius:0;border-bottom:1px solid var(--line)}
.sec{display:flex;flex-direction:column;gap:.6rem}
/* Group divider inside a sheet: names the publisher and counts its rows. */
.grp{display:flex;align-items:center;gap:.5rem;padding:.4rem .9rem;
  background:var(--bg);border-bottom:1px solid var(--line);
  color:var(--dim);font-size:.75rem;font-weight:600;letter-spacing:-.005em}
.grp .cnt{color:var(--dim-2);font-weight:500;font-variant-numeric:tabular-nums}
.cols{display:grid;grid-template-columns:repeat(auto-fit,minmax(19rem,1fr));gap:.7rem;
  align-items:start}

/* ---- section headings (standalone, outside a sheet) ---- */
h2{color:var(--fg);font-size:.86rem;letter-spacing:-.01em;margin:0;
  font-weight:620;display:flex;align-items:center;gap:.7rem}
h2::after{content:"";flex:1;height:1px;background:var(--line)}
h3{letter-spacing:-.01em;font-size:.85rem;margin:0 0 .4rem}

/* ---- controls ---- */
input,.card input,.card select,.form-grid input,.form-grid select,
.settings-card select,.settings-card input:not([type=checkbox]),.mm-row input,.hdr-row input,
.tools input{
  background:var(--bg);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.44rem .6rem;font:inherit;
  transition:border-color .12s,box-shadow .12s}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);
  box-shadow:0 0 0 3px rgba(91,141,239,.18)}
input::placeholder{color:var(--dim-2)}
button{background:var(--panel-2);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.4rem .75rem;font:inherit;font-size:.8rem;
  font-weight:550;cursor:pointer;transition:background .12s,border-color .12s;white-space:nowrap}
button:hover{background:var(--line);border-color:var(--line-2)}
button:focus-visible,.sw:focus-visible,input[type=checkbox]:focus-visible{
  outline:2px solid var(--accent);outline-offset:2px}
button:disabled{opacity:.45;cursor:not-allowed}
/* One inverted button per sheet marks the action that commits that sheet.
   Everything else stays a quiet secondary control. */
button.primary{background:var(--fg);border-color:var(--fg);color:#0c1013;font-weight:620}
button.primary:hover{background:#fff;border-color:#fff}
#all,#none,#retry{background:transparent}
.btn-sm{padding:.26rem .55rem;font-size:.75rem}
.btn-del{color:var(--bad);border-color:color-mix(in srgb,var(--bad) 40%,var(--line))}
.btn-del:hover{background:rgba(224,112,111,.12);border-color:var(--bad)}
.test-res{font-size:.78rem;display:inline-flex;align-items:center;gap:.4rem}
.dim{color:var(--dim)}

/* ---- rows ---- */
.row{display:flex;align-items:center;gap:.7rem;padding:.5rem .9rem;
  background:var(--panel);cursor:pointer;transition:background .12s}
.row:hover{background:var(--panel-2)}
.row.hide{display:none}
/* .row is shared chrome: model toggles, provider rows, captcha workers. Only
   model-toggle rows carry data-id + .sw; every models-only script hook keys on
   that pair so other pages are left alone. */
.row.static{cursor:default}
.name{flex:1;display:flex;align-items:center;gap:.5rem;min-width:0;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.id{color:var(--dim);font-size:.73rem;font-family:var(--mono)}

/* ---- badges ---- */
/* Status is carried by a word plus a shape, not by hue alone: a dot is a second
   channel for a red-green colorblind operator. The dot is a box for states that
   are not live (killed, provider tags) and a circle for live ones. */
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
.chip{font-family:var(--mono);font-size:.66rem;color:var(--dim);
  border:1px solid var(--line-2);border-radius:4px;padding:.05rem .3rem}

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
  font-size:1.05rem;padding:0 .35rem;border-radius:6px;line-height:1;cursor:pointer;
  opacity:0;transition:opacity .15s,color .15s}
.row:hover .del-btn,.del-btn:focus-visible{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad);background:none}

/* ---- cards & forms ---- */
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));
  gap:.5rem;padding:.85rem;border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);margin:0}
.card button{white-space:nowrap}
.settings-card{display:block;padding:.9rem}
.settings-card p{color:var(--dim);font-size:.8rem;margin:.1rem 0 .9rem;max-width:52rem;
  line-height:1.6}
.settings-card p:last-child{margin-bottom:0}
.settings-card code{font-family:var(--mono);font-size:.76rem;color:var(--fg);
  background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:.05rem .3rem}
.settings-card select,.settings-card input:not([type=checkbox]){display:block;
  width:min(100%,32rem);margin:.3rem 0 .8rem}
.settings-card label{color:var(--dim);font-size:.75rem;font-weight:550}
.form-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(13rem,1fr));
  gap:.6rem .7rem;margin-bottom:.6rem}
.form-grid label,.form-grid input,.form-grid select{display:block;width:100%}
.form-grid label{color:var(--dim);font-size:.75rem;font-weight:550;margin-bottom:.2rem}
.mm-row,.hdr-row{display:flex;gap:.4rem;align-items:center;margin-bottom:.3rem}
.mm-row input,.hdr-row input{font-size:.8rem;min-width:0;padding:.34rem .5rem}
.mm-row input.mm-alias{flex:1.2}
.mm-row input.mm-upstream{flex:1.5}
.mm-row input.mm-fallbacks{flex:1.3}
.mm-row label{display:flex;align-items:center;gap:.25rem;color:var(--dim);
  font-size:.73rem;white-space:nowrap;margin:0 .2rem}
.hdr-row input.hdr-name{flex:1}
.hdr-row input.hdr-val{flex:1.5}
.del-row-btn{background:none;border:none;color:var(--dim);font-size:1.1rem;
  cursor:pointer;padding:0 .3rem;line-height:1}
.del-row-btn:hover{color:var(--bad);background:none}
.bar{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap}

/* ---- provider rows ---- */
.prov-card{background:var(--panel);padding:.65rem .9rem;transition:background .12s}
.prov-card:hover{background:var(--panel-2)}
.prov-head{display:flex;align-items:center;gap:.55rem;flex-wrap:wrap}
.prov-head strong{letter-spacing:-.01em;font-weight:600}
.prov-meta{color:var(--dim-2);font-size:.74rem;font-family:var(--mono);
  margin-left:auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  max-width:45%}
.prov-aliases{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.45rem}
.prov-actions{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.5rem}
.prov-card .sw{margin-left:.2rem}
.alias-lbl{color:var(--dim-2);font-size:.72rem;font-weight:550}

/* ---- cycle list: an ordered priority list, so it is numbered ---- */
.cycle-list{border:1px solid var(--line);border-radius:var(--r-sm);margin:.3rem 0 .8rem;
  background:var(--bg);max-height:20rem;overflow:auto}
.cycle-option{display:flex;align-items:center;gap:.5rem;padding:.3rem .5rem;
  border-bottom:1px solid var(--line)}
.cycle-option:last-child{border-bottom:0}
.cycle-option .pos{color:var(--dim-2);font-family:var(--mono);font-size:.7rem;
  min-width:1.2rem;text-align:right}
.cycle-option label{display:flex;align-items:center;gap:.5rem;flex:1;
  overflow-wrap:anywhere;color:var(--fg);font-size:.8rem}
.cycle-option input[type=checkbox]{flex:none;width:.95rem;height:.95rem;margin:0;padding:0;
  accent-color:var(--on)}
.cycle-option button{padding:.05rem .35rem;font-size:.75rem}

/* ---- toast ---- */
#toast{position:fixed;right:1.2rem;bottom:1.2rem;padding:.55rem .85rem;
  border-radius:var(--r);border:1px solid var(--line-2);background:var(--panel-2);
  box-shadow:0 10px 30px rgba(0,0,0,.5);opacity:0;transform:translateY(6px);
  transition:opacity .18s,transform .18s;pointer-events:none;font-size:.8rem}
#toast.show{opacity:1;transform:none}

/* ---- dashboard ---- */
/* A reading board, not eight identical cards: one continuous sheet divided by
   hairlines, so the numbers compare down a column instead of floating apart. */
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5rem,1fr));
  gap:1px;background:var(--line);border:1px solid var(--line);
  border-radius:var(--r-lg);overflow:hidden}
.tile{background:var(--panel);padding:.7rem .9rem .65rem}
.tile .lbl{color:var(--dim);font-size:.73rem;margin-bottom:.1rem;font-weight:500}
.tile .big{font-size:1.45rem;font-weight:600;line-height:1.2;letter-spacing:-.03em;
  font-family:var(--mono);font-variant-numeric:tabular-nums}
.tile svg{margin-top:.4rem}
.chartbox{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  padding:.65rem .85rem .5rem}
.chartrow{display:grid;grid-template-columns:repeat(auto-fit,minmax(24rem,1fr));
  gap:.7rem}
.note{color:var(--dim-2);font-size:.73rem;margin:.2rem 0;max-width:60rem}
#frames{display:flex;gap:.25rem;flex-wrap:wrap;margin-left:auto}
.grp.hide{display:none}
#frames button{padding:.15rem .5rem;font-size:.74rem;font-weight:500}
.fp.on{background:var(--line-2);border-color:var(--line-2);color:var(--fg)}
.logwrap{max-height:22rem;overflow:auto}
.chhead{display:flex;gap:1rem;margin-bottom:.35rem;flex-wrap:wrap}
.lg{color:var(--dim);font-size:.71rem;display:inline-flex;align-items:center;gap:.3rem}
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
table.stats{width:100%;border-collapse:collapse;font-size:.78rem;
  font-variant-numeric:tabular-nums}
table.stats th,table.stats td{padding:.42rem .6rem;border-bottom:1px solid var(--line);
  text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);
  background:var(--panel-2);position:sticky;top:0;font-weight:550;font-size:.73rem}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" ↑"}
table.stats thead th.on:not(.asc)::after{content:" ↓"}
table.stats td.nm{text-align:left;font-family:var(--mono);font-size:.73rem}
table.stats tbody tr:hover{background:var(--panel-2)}
table.stats tbody tr:last-child td{border-bottom:0}

/* ---- deleted models ---- */
#deleted-section{border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);overflow:hidden}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.6rem .9rem;
  cursor:pointer;user-select:none;font-size:.8rem;font-weight:550}
#deleted-header:hover{background:var(--panel-2)}
#deleted-header .cnt{color:var(--warn);font-size:.75rem;font-weight:600}
#deleted-header .arrow{color:var(--dim);font-size:.68rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.4rem .9rem;
  background:var(--panel)}
.del-row .id{flex:1}
.del-row button{border-color:color-mix(in srgb,var(--on) 40%,var(--line));
  color:var(--on);padding:.18rem .5rem;font-size:.75rem}
.del-row button:hover{background:rgba(63,178,127,.12);border-color:var(--on)}

/* ---- narrow screens ---- */
@media (max-width:640px){
  .wrap,main{padding-left:.9rem;padding-right:.9rem}
  .brand .product{display:none}
  .head-row{gap:.6rem}
  .tab-bar{display:flex;width:100%}
  .tab{flex:1;text-align:center}
  .tools .sub{margin-left:0}
  input[type=search]{max-width:none}
  .prov-meta{display:none}
  .chartrow{grid-template-columns:1fr}
  .row{padding:.5rem .7rem}
}
</style>
<header><div class=wrap>
  <div class=head-row>
    <div class=brand><h1>` + title + `</h1><span class=product>Gateway admin</span></div>
    <nav class=tab-bar aria-label="Admin sections">` + adminHeadTabs(active) + `</nav>
  </div>
  ` + adminHeadSub(active) + `
</div></header>
`
}
