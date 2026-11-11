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
	return `<div class=rail-links>` + b.String() + `</div>`
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
  color-scheme:light;
  /* A light workspace: white panels on a cool paper plane, ink text, one blue
     for action, and three state colours dark enough to read on white. Chart
     series live in the stats script and are tuned to this same surface. */
  --bg:#f4f5f7;--bg-2:#eceff3;--panel:#ffffff;--panel-2:#f7f8fa;
  --line:#e2e6eb;--line-2:#cbd3dc;
  --fg:#1b2430;--dim:#5c6876;--dim-2:#7f8b98;
  --on:#12734a;--bad:#b42318;--warn:#8a5a00;--accent:#1f5fd0;--on-accent:#fff;
  --r-sm:6px;--r:9px;--r-lg:12px;
  --shadow:0 1px 2px rgba(16,24,40,.05),0 1px 3px rgba(16,24,40,.06);
  --mono:ui-monospace,"SF Mono","Cascadia Mono",Consolas,monospace;
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
@media (prefers-reduced-motion:reduce){
  *,*::before,*::after{animation-duration:.01ms!important;animation-iteration-count:1!important;
    transition-duration:.01ms!important;scroll-behavior:auto!important}
}
body{margin:0;padding-left:13rem;background:var(--bg);color:var(--fg);
  font:14px/1.55 ui-sans-serif,system-ui,"Segoe UI",Roboto,sans-serif;
  -webkit-font-smoothing:antialiased}
::selection{background:rgba(31,95,208,.18)}
a{color:var(--accent)}

/* ---- shell ---- */
header{position:sticky;top:0;z-index:20;background:var(--panel);
  border-bottom:1px solid var(--line);box-shadow:0 1px 0 rgba(16,24,40,.02)}
.wrap,main{max-width:72rem;margin:0 auto;padding:0 1.5rem}
.head-row{display:flex;align-items:center;gap:1rem;flex-wrap:wrap;padding:.7rem 0 .65rem}
.brand{display:flex;align-items:baseline;gap:.55rem;margin-right:auto}
h1{margin:0;font-size:1.2rem;font-weight:650;letter-spacing:-.022em}
.brand .product{color:var(--dim-2);font-size:.75rem;font-weight:450;letter-spacing:.01em}
.tools{display:flex;gap:.4rem;align-items:center;flex-wrap:wrap;padding:0 0 .7rem}
.tools .sub{margin:0 0 0 auto}
input[type=search]{flex:1;min-width:12rem;max-width:22rem}
.sub{color:var(--dim);font-size:.79rem;padding:0 0 .7rem}

/* ---- left rail: the section switcher lives in a fixed column, so the
   content area gets the full width and the current section is always in
   view instead of scrolling away with the page. ---- */
.rail{position:fixed;inset:0 auto 0 0;width:13rem;z-index:30;display:flex;
  flex-direction:column;gap:.35rem;padding:1rem .7rem;
  background:var(--panel);border-right:1px solid var(--line)}
.rail-brand{display:flex;align-items:center;gap:.55rem;padding:.15rem .35rem 1rem}
.rail-brand .mark{width:1.65rem;height:1.65rem;border-radius:8px;background:var(--accent);
  color:var(--on-accent);display:grid;place-items:center;font-size:.78rem;font-weight:700}
.rail-brand .rail-name{font-weight:650;letter-spacing:-.02em;font-size:.92rem}
.rail-links{display:flex;flex-direction:column;gap:2px}
.tab{color:var(--dim);font-size:.84rem;font-weight:550;padding:.42rem .65rem;
  border-radius:var(--r-sm);text-decoration:none;transition:background .12s,color .12s}
.tab:hover{color:var(--fg);background:var(--bg-2)}
.tab.on{color:var(--on-accent);background:var(--accent);box-shadow:var(--shadow)}
.tab.on:hover{color:var(--on-accent);background:var(--accent)}
.tab:focus-visible{outline:2px solid var(--accent);outline-offset:2px}

main{padding:1.5rem 1.5rem 5rem;display:flex;flex-direction:column;gap:.9rem}

/* ---- sheets: the one panel container ---- */
.sheet{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  box-shadow:var(--shadow);overflow:hidden}
.sheet>.row,.sheet>.prov-card,.sheet>.del-row{border:0;border-radius:0;margin:0;
  border-bottom:1px solid var(--line)}
.sheet>:last-child{border-bottom:0}
.sheet-head{display:flex;align-items:center;gap:.7rem;flex-wrap:wrap;
  padding:.6rem 1rem;background:var(--panel-2);border-bottom:1px solid var(--line)}
.sheet-head h2{margin:0;font-size:.88rem}
.sheet-head h2::after{content:none}
.sheet-head .sub{padding:0;margin-left:auto}
.sheet-title{font-size:.88rem;font-weight:620;letter-spacing:-.012em}
.sheet-body{padding:1rem}
.sheet-foot{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap;
  padding:.65rem 1rem;border-top:1px solid var(--line);background:var(--panel-2)}
details.sheet>summary{list-style:none;cursor:pointer;user-select:none}
details.sheet>summary::-webkit-details-marker{display:none}
details.sheet>summary::after{content:"+";margin-left:auto;color:var(--dim);
  font-size:1rem;line-height:1;font-weight:600}
details.sheet[open]>summary::after{content:"\2013"}
details.sheet>summary:hover{background:var(--bg-2)}
.sheet .tiles{border:0;border-radius:0;border-bottom:1px solid var(--line);box-shadow:none}
.sec{display:flex;flex-direction:column;gap:.65rem}
.grp{display:flex;align-items:center;gap:.5rem;padding:.45rem 1rem;
  background:var(--bg);border-bottom:1px solid var(--line);
  color:var(--dim);font-size:.76rem;font-weight:600;letter-spacing:-.005em}
.grp .cnt{color:var(--dim-2);font-weight:500;font-variant-numeric:tabular-nums}
.cols{display:grid;grid-template-columns:repeat(auto-fit,minmax(19rem,1fr));gap:.9rem;
  align-items:start}
/* Models is the one page with two jobs: providers are edited, the catalog is
   scanned. They sit side by side instead of stacking into a long scroll. */
.split{display:grid;grid-template-columns:minmax(19rem,25rem) minmax(0,1fr);
  gap:.9rem;align-items:start}
.split>.pane{min-width:0;display:flex;flex-direction:column;gap:.9rem}
@media (max-width:1100px){.split{grid-template-columns:1fr}}

/* ---- section headings (standalone, outside a sheet) ---- */
h2{color:var(--fg);font-size:.88rem;letter-spacing:-.012em;margin:0;
  font-weight:620;display:flex;align-items:center;gap:.7rem}
h2::after{content:"";flex:1;height:1px;background:var(--line)}
h3{letter-spacing:-.012em;font-size:.86rem;margin:0 0 .4rem}

/* ---- controls ---- */
input,.card input,.card select,.form-grid input,.form-grid select,
.settings-card select,.settings-card input:not([type=checkbox]),.mm-row input,.hdr-row input,
.tools input{
  background:var(--panel);border:1px solid var(--line-2);color:var(--fg);
  border-radius:var(--r-sm);padding:.46rem .62rem;font:inherit;
  transition:border-color .12s,box-shadow .12s}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);
  box-shadow:0 0 0 3px rgba(31,95,208,.16)}
input::placeholder{color:var(--dim-2)}
button{background:var(--panel);border:1px solid var(--line-2);color:var(--fg);
  border-radius:var(--r-sm);padding:.42rem .8rem;font:inherit;font-size:.8rem;
  font-weight:550;cursor:pointer;transition:background .12s,border-color .12s;
  white-space:nowrap;box-shadow:0 1px 1px rgba(16,24,40,.03)}
button:hover{background:var(--panel-2);border-color:var(--dim-2)}
button:focus-visible,.sw:focus-visible,input[type=checkbox]:focus-visible{
  outline:2px solid var(--accent);outline-offset:2px}
button:disabled{opacity:.45;cursor:not-allowed}
/* One filled button per sheet marks the action that commits that sheet. */
button.primary{background:var(--accent);border-color:var(--accent);
  color:var(--on-accent);font-weight:620;box-shadow:var(--shadow)}
button.primary:hover{background:#1a52b8;border-color:#1a52b8}
#all,#none,#retry{background:var(--panel)}
.btn-sm{padding:.28rem .58rem;font-size:.76rem}
.btn-del{color:var(--bad);border-color:#e7b6b1}
.btn-del:hover{background:#fdf2f1;border-color:var(--bad)}
.test-res{font-size:.79rem;display:inline-flex;align-items:center;gap:.4rem}
.dim{color:var(--dim)}

/* ---- rows ---- */
.row{display:flex;align-items:center;gap:.7rem;padding:.55rem 1rem;
  background:var(--panel);cursor:pointer;transition:background .12s}
.row:hover{background:var(--panel-2)}
.row.hide{display:none}
.row.static{cursor:default}
.name{flex:1;display:flex;align-items:center;gap:.5rem;min-width:0;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.id{color:var(--dim);font-size:.74rem;font-family:var(--mono)}

/* ---- badges ---- */
/* Status is a word plus a shape, never hue alone. */
.badge{color:var(--bad);font-size:.69rem;font-weight:550;line-height:1.5;
  padding:0 .1rem;white-space:nowrap;display:inline-flex;align-items:center;gap:.34rem}
.badge::before{content:"";width:6px;height:6px;flex:none;border-radius:50%;
  background:currentColor}
.badge.sq::before{border-radius:1px}
.badge.ok{color:var(--on)}
.badge.warn{color:var(--warn)}
.badge.mute{color:var(--dim-2)}
.badge.pb{color:var(--on);opacity:.9;font-family:var(--mono);font-size:.67rem;
  border:1px solid var(--line-2);border-radius:4px;padding:0 .3rem;background:var(--panel-2)}
.badge.pb::before{display:none}
.chip{font-family:var(--mono);font-size:.67rem;color:var(--dim);
  border:1px solid var(--line-2);border-radius:4px;padding:.05rem .3rem;
  background:var(--panel-2)}

/* ---- switch ---- */
.sw{appearance:none;flex:none;width:38px;height:22px;border:0;border-radius:999px;
  background:var(--line-2);position:relative;cursor:pointer;transition:background .15s;
  margin:0;padding:0}
.sw:checked{background:var(--on)}
.sw::after{content:"";position:absolute;top:3px;left:3px;width:16px;height:16px;
  border-radius:50%;background:#fff;transition:transform .15s;
  box-shadow:0 1px 2px rgba(16,24,40,.35)}
.sw:checked::after{transform:translateX(16px)}
.row:has(.sw:not(:checked)) .name{color:var(--dim);text-decoration:line-through;
  text-decoration-color:var(--dim-2)}
.del-btn{flex:none;background:none;border:1px solid transparent;color:var(--dim);
  font-size:1.05rem;padding:0 .35rem;border-radius:6px;line-height:1;cursor:pointer;
  opacity:0;transition:opacity .15s,color .15s;box-shadow:none}
.row:hover .del-btn,.del-btn:focus-visible{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad);background:#fdf2f1}

/* ---- cards & forms ---- */
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));
  gap:.5rem;padding:.9rem;border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);margin:0;box-shadow:var(--shadow)}
.card button{white-space:nowrap}
.settings-card{display:block;padding:1rem}
.settings-card p{color:var(--dim);font-size:.81rem;margin:.1rem 0 .9rem;max-width:52rem;
  line-height:1.6}
.settings-card p:last-child{margin-bottom:0}
.settings-card code{font-family:var(--mono);font-size:.77rem;color:var(--fg);
  background:var(--bg-2);border:1px solid var(--line);border-radius:4px;padding:.05rem .3rem}
.settings-card select,.settings-card input:not([type=checkbox]){display:block;
  width:min(100%,32rem);margin:.3rem 0 .8rem}
.settings-card label{color:var(--dim);font-size:.76rem;font-weight:550}
.form-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(13rem,1fr));
  gap:.6rem .7rem;margin-bottom:.6rem}
.form-grid label,.form-grid input,.form-grid select{display:block;width:100%}
.form-grid label{color:var(--dim);font-size:.76rem;font-weight:550;margin-bottom:.2rem}
.mm-row,.hdr-row{display:flex;gap:.4rem;align-items:center;margin-bottom:.3rem}
.mm-row input,.hdr-row input{font-size:.8rem;min-width:0;padding:.34rem .5rem}
.mm-row input.mm-alias{flex:1.2}
.mm-row input.mm-upstream{flex:1.5}
.mm-row input.mm-fallbacks{flex:1.3}
.mm-row label{display:flex;align-items:center;gap:.25rem;color:var(--dim);
  font-size:.74rem;white-space:nowrap;margin:0 .2rem}
.hdr-row input.hdr-name{flex:1}
.hdr-row input.hdr-val{flex:1.5}
.del-row-btn{background:none;border:none;color:var(--dim);font-size:1.1rem;
  cursor:pointer;padding:0 .3rem;line-height:1;box-shadow:none}
.del-row-btn:hover{color:var(--bad);background:none}
.bar{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap}

/* ---- provider rows ---- */
.prov-card{background:var(--panel);padding:.7rem 1rem;transition:background .12s}
.prov-card:hover{background:var(--panel-2)}
.prov-head{display:flex;align-items:center;gap:.55rem;flex-wrap:wrap}
.prov-head strong{letter-spacing:-.012em;font-weight:600}
.prov-meta{color:var(--dim-2);font-size:.75rem;font-family:var(--mono);
  margin-left:auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  max-width:45%}
.prov-aliases{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.5rem}
.prov-actions{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.55rem}
.prov-card .sw{margin-left:.2rem}
.alias-lbl{color:var(--dim-2);font-size:.73rem;font-weight:550}

/* ---- cycle list: an ordered priority list, so it is numbered ---- */
.cycle-list{border:1px solid var(--line);border-radius:var(--r-sm);margin:.3rem 0 .8rem;
  background:var(--panel);max-height:20rem;overflow:auto}
.cycle-option{display:flex;align-items:center;gap:.5rem;padding:.34rem .55rem;
  border-bottom:1px solid var(--line)}
.cycle-option:last-child{border-bottom:0}
.cycle-option .pos{color:var(--dim-2);font-family:var(--mono);font-size:.71rem;
  min-width:1.2rem;text-align:right}
.cycle-option label{display:flex;align-items:center;gap:.5rem;flex:1;
  overflow-wrap:anywhere;color:var(--fg);font-size:.8rem}
.cycle-option input[type=checkbox]{flex:none;width:.95rem;height:.95rem;margin:0;padding:0;
  accent-color:var(--on)}
.cycle-option button{padding:.05rem .35rem;font-size:.76rem}

/* ---- toast ---- */
#toast{position:fixed;right:1.2rem;bottom:1.2rem;padding:.6rem .9rem;
  border-radius:var(--r);border:1px solid var(--line-2);background:var(--fg);
  color:#fff;box-shadow:0 10px 30px rgba(16,24,40,.22);opacity:0;transform:translateY(6px);
  transition:opacity .18s,transform .18s;pointer-events:none;font-size:.8rem}
#toast.show{opacity:1;transform:none}

/* ---- dashboard ---- */
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5rem,1fr));
  gap:1px;background:var(--line);border:1px solid var(--line);
  border-radius:var(--r-lg);overflow:hidden;box-shadow:var(--shadow)}
.tile{background:var(--panel);padding:.75rem 1rem .7rem}
.tile .lbl{color:var(--dim);font-size:.74rem;margin-bottom:.1rem;font-weight:500}
.tile .big{font-size:1.5rem;font-weight:600;line-height:1.2;letter-spacing:-.03em;
  font-family:var(--mono);font-variant-numeric:tabular-nums}
.tile svg{margin-top:.4rem}
.chartbox{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  padding:.7rem .9rem .55rem;box-shadow:var(--shadow)}
.chartrow{display:grid;grid-template-columns:repeat(auto-fit,minmax(24rem,1fr));
  gap:.9rem}
.note{color:var(--dim-2);font-size:.74rem;margin:.2rem 0;max-width:60rem}
#frames{display:flex;gap:.25rem;flex-wrap:wrap;margin-left:auto}
.grp.hide{display:none}
#frames button{padding:.16rem .52rem;font-size:.75rem;font-weight:500}
.fp.on{background:var(--accent);border-color:var(--accent);color:var(--on-accent)}
.logwrap{max-height:22rem;overflow:auto}
.chhead{display:flex;gap:1rem;margin-bottom:.35rem;flex-wrap:wrap}
.lg{color:var(--dim);font-size:.72rem;display:inline-flex;align-items:center;gap:.3rem}
.chwrap{position:relative}
.ax{fill:var(--dim-2);font-size:9px;font-family:var(--mono)}
.tip{position:absolute;pointer-events:none;background:var(--panel);
  border:1px solid var(--line-2);border-radius:var(--r-sm);padding:.5rem .65rem;
  min-width:120px;font-size:.73rem;box-shadow:0 8px 24px rgba(16,24,40,.16);z-index:5}
.tip.hidden{display:none}
.tip-h{color:var(--dim);margin-bottom:.3rem;font-family:var(--mono);font-size:.71rem}
.tip-r{display:flex;justify-content:space-between;gap:1rem;line-height:1.6}
.tip-k{color:var(--dim);display:inline-flex;align-items:center;gap:.35rem}
.tip-sw{width:10px;height:2px;border-radius:1px;display:inline-block}
.tip-r b{font-weight:600;font-variant-numeric:tabular-nums}
table.stats{width:100%;border-collapse:collapse;font-size:.79rem;
  font-variant-numeric:tabular-nums}
table.stats th,table.stats td{padding:.45rem .65rem;border-bottom:1px solid var(--line);
  text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);
  background:var(--panel-2);position:sticky;top:0;font-weight:550;font-size:.74rem}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" \2191"}
table.stats thead th.on:not(.asc)::after{content:" \2193"}
table.stats td.nm{text-align:left;font-family:var(--mono);font-size:.74rem}
table.stats tbody tr:hover{background:var(--panel-2)}
table.stats tbody tr:last-child td{border-bottom:0}

/* ---- deleted models ---- */
#deleted-section{border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);overflow:hidden;box-shadow:var(--shadow)}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.65rem 1rem;
  cursor:pointer;user-select:none;font-size:.81rem;font-weight:550}
#deleted-header:hover{background:var(--panel-2)}
#deleted-header .cnt{color:var(--warn);font-size:.76rem;font-weight:600}
#deleted-header .arrow{color:var(--dim);font-size:.69rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.45rem 1rem;
  background:var(--panel)}
.del-row .id{flex:1}
.del-row button{border-color:#b9dcc9;color:var(--on);padding:.2rem .52rem;font-size:.76rem}
.del-row button:hover{background:#f0faf4;border-color:var(--on)}

/* ---- narrow screens ---- */
/* Below the rail breakpoint the navigation becomes a top strip again. */
@media (max-width:900px){
  body{padding-left:0}
  .rail{position:static;width:auto;flex-direction:row;align-items:center;
    gap:.6rem;padding:.55rem .9rem;border-right:0;border-bottom:1px solid var(--line)}
  .rail-brand{padding:0}
  .rail-links{flex-direction:row;flex-wrap:wrap}
  .rail-brand .rail-name{display:none}
}
@media (max-width:640px){
  .wrap,main{padding-left:.9rem;padding-right:.9rem}
  .tab{flex:1;text-align:center}
  .tools .sub{margin-left:0}
  input[type=search]{max-width:none}
  .prov-meta{display:none}
  .chartrow{grid-template-columns:1fr}
  .row{padding:.5rem .75rem}
}
</style>
<nav class=rail aria-label="Admin sections">
  <div class=rail-brand><span class=mark>GW</span><span class=rail-name>Gateway</span></div>
  ` + adminHeadTabs(active) + `
</nav>
<header><div class=wrap>
  <h1>` + title + `</h1>
  ` + adminHeadSub(active) + `
</div></header>
`
}
