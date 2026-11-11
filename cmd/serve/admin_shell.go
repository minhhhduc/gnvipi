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

// adminHeadTabs renders the nav bar, marking the active tab with .on. The tabs
// are text with a signal underline rather than pills, so the header stays a
// single quiet band and the accent is spent on the one word you are on.
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
	"dashboard": `<div class=sub>Requests, latency and tokens across every upstream model.</div>`,
	"config":    `<div class=sub>Pick the auto model, the web search engine, and the failover order.</div>`,
	"chromes":   `<div class=sub>Captcha browser workers: start on demand, kill one to free its RAM.</div>`,
}

// adminHead renders the shared page shell: the stylesheet, the header strip
// (brand, tabs, page toolbar) and nothing else. adminHTML appends <main>.
//
// Stylesheet notes: the page is server-rendered with inline CSS and JS, so
// every rule here has to stand on its own — no CDN, no build step, no external
// font. Layout is built from two containers: .sheet (a bordered panel whose
// children are divided by hairlines, used for anything list-shaped) and plain
// sections for prose and charts.
//
// Identity notes: the palette is a warm graphite casing with one cool signal
// (teal), not the usual black/grey/blue. The header carries a short signal rail;
// each sheet head carries a small signal tick; everything else is hairlines and
// spacing. Type comes from the system stack, so the scale itself does the work.
func adminHead(active string) string {
	title := adminTitle[active]
	if title == "" {
		title = "Models"
	}
	return `<!doctype html><meta charset="utf-8"><title>` + title + ` · Gateway admin</title>
<meta name=viewport content="width=device-width,initial-scale=1">
<style>
:root{
  /* Warm graphite instrument casing with a single cool signal. Contrast was
     checked against --panel: fg 14.9:1, dim 8.0:1, dim-2 5.0:1, and each state
     (on 8.2, warn 8.0, bad 5.4, accent 8.1). The chart series in the stats
     script were re-stepped and validated against --panel; keep it stable. */
  --bg:#12100d;--bg-2:#171410;--panel:#1e1a15;--panel-2:#26211b;
  --line:#3a322a;--line-2:#4d4237;
  --fg:#f4ede3;--dim:#bdae9d;--dim-2:#978775;
  --on:#63c76a;--bad:#e26d5c;--warn:#e2a63c;--accent:#2fc4c0;
  --on-accent:#0f1a12;
  --r-sm:5px;--r:9px;--r-lg:13px;
  /* Type scale. With no webfont allowed, hierarchy comes from size, weight and
     tracking on the system stack — not from a display face. */
  --fs-2xs:.6875rem;--fs-xs:.75rem;--fs-sm:.8125rem;--fs-md:.875rem;
  --fs-lg:1.0625rem;--fs-xl:1.3125rem;--fs-2xl:1.75rem;
  --lh-tight:1.15;--lh-snug:1.35;--lh-base:1.55;
  --fw-normal:400;--fw-medium:500;--fw-semi:600;--fw-bold:700;
  --track-tight:-.015em;--track-micro:.02em;
  --sp-1:.25rem;--sp-2:.5rem;--sp-3:.75rem;--sp-4:1rem;--sp-5:1.5rem;
  /* Mono is reserved for identifiers and numbers compared row to row. */
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
  font:var(--fw-normal) var(--fs-md)/var(--lh-base) ui-sans-serif,system-ui,"Segoe UI",Roboto,sans-serif;
  -webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
::selection{background:color-mix(in srgb,var(--accent) 32%,transparent);color:var(--fg)}
a{color:var(--accent);text-underline-offset:2px}

/* ---- shell ---- */
header{position:sticky;top:0;z-index:20;
  background:color-mix(in srgb,var(--bg-2) 88%,transparent);
  backdrop-filter:blur(14px) saturate(1.15);
  border-bottom:1px solid var(--line)}
/* The signal rail: the one mark that repeats, a short live-looking line on the
   header edge. It fades out rather than running the full width. */
header::before{content:"";position:absolute;inset:0 0 auto 0;height:2px;
  background:linear-gradient(90deg,var(--accent),color-mix(in srgb,var(--accent) 16%,transparent) 42%,transparent)}
.wrap,main{max-width:64rem;margin:0 auto;padding:0 var(--sp-5)}
.head-row{display:flex;align-items:center;gap:var(--sp-5);flex-wrap:wrap;
  padding:var(--sp-4) 0 var(--sp-3)}
/* The page you are on is the h1; no section heading below repeats it back. */
.brand{display:flex;align-items:center;gap:.6rem;margin-right:auto;min-width:0}
.brand-mark{display:inline-flex;flex:none;color:var(--accent)}
.brand-mark svg{display:block}
.wordmark{font-size:var(--fs-2xs);font-weight:var(--fw-semi);letter-spacing:var(--track-micro);
  color:var(--dim);text-transform:lowercase;white-space:nowrap}
.brand-sep{width:1px;height:1.15em;flex:none;background:var(--line-2)}
h1{margin:0;font-size:var(--fs-xl);font-weight:var(--fw-bold);letter-spacing:var(--track-tight);
  line-height:var(--lh-tight);color:var(--fg)}
/* The toolbar belongs to the header strip so it stays glued to the top on the
   one page that scrolls through hundreds of rows. */
.tools{display:flex;gap:var(--sp-2);align-items:center;flex-wrap:wrap;
  padding:0 0 var(--sp-3)}
.tools .sub{margin:0 0 0 auto}
input[type=search]{flex:1;min-width:12rem;max-width:22rem}
.sub{color:var(--dim);font-size:var(--fs-xs);padding:0 0 var(--sp-3);line-height:var(--lh-snug)}

/* ---- nav tabs: text with a signal underline ---- */
.tab-bar{display:inline-flex;align-items:center;gap:var(--sp-4)}
.tab{position:relative;color:var(--dim);font-size:var(--fs-sm);font-weight:var(--fw-medium);
  padding:.35rem 0;text-decoration:none;letter-spacing:-.005em;transition:color .12s}
.tab:hover{color:var(--fg)}
.tab.on{color:var(--fg)}
.tab.on::after{content:"";position:absolute;left:0;right:0;bottom:-.45rem;height:2px;
  border-radius:2px;background:var(--accent)}
.tab:focus-visible{outline:2px solid var(--accent);outline-offset:3px;border-radius:2px}

main{padding:var(--sp-5) var(--sp-5) 5rem;display:flex;flex-direction:column;gap:var(--sp-3)}

/* ---- sheets: the one list container ---- */
/* A sheet is a bordered panel whose children are divided by hairlines. Rows
   keep their own look; only the container changes. Providers, workers, the
   model catalog and the request log all share it so the eye learns one rule. */
.sheet{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  overflow:hidden;box-shadow:inset 0 1px 0 rgba(255,255,255,.03)}
.sheet>.row,.sheet>.prov-card,.sheet>.del-row{border:0;border-radius:0;margin:0;
  border-bottom:1px solid var(--line)}
.sheet>:last-child{border-bottom:0}
.sheet-head{display:flex;align-items:center;gap:var(--sp-3);flex-wrap:wrap;
  padding:var(--sp-2) var(--sp-4);background:var(--panel-2);border-bottom:1px solid var(--line)}
.sheet-head h2{margin:0;font-size:var(--fs-sm);font-weight:var(--fw-semi);letter-spacing:-.005em}
.sheet-head h2::after{content:none}
.sheet-head .sub{padding:0;margin-left:auto}
.sheet-title{font-size:var(--fs-sm);font-weight:var(--fw-semi);letter-spacing:-.005em}
/* The signal tick: every panel head gets the same short accent mark, so a
   sheet reads as a sheet before you read its title. */
.sheet-head h2::before,.sheet-title::before{content:"";display:inline-block;flex:none;
  width:3px;height:.92em;margin-right:.5rem;border-radius:1px;
  background:var(--accent);vertical-align:-.1em}
.sheet-body{padding:var(--sp-4)}
.sheet-foot{display:flex;gap:var(--sp-2);align-items:center;flex-wrap:wrap;
  padding:var(--sp-2) var(--sp-4);border-top:1px solid var(--line);background:var(--panel-2)}
/* A <details> used as a sheet: the summary is the sheet header. */
details.sheet>summary{list-style:none;cursor:pointer;user-select:none}
details.sheet>summary::-webkit-details-marker{display:none}
details.sheet>summary::after{content:"+";margin-left:auto;color:var(--dim);
  font-size:1rem;line-height:1}
details.sheet[open]>summary::after{content:"–"}
details.sheet>summary:hover{background:var(--line)}
/* Tiles already carry their own sheet; nested inside one they only add a rule. */
.sheet .tiles{border:0;border-radius:0;border-bottom:1px solid var(--line)}
.sec{display:flex;flex-direction:column;gap:var(--sp-2)}
/* Group divider inside a sheet: names the publisher and counts its rows. */
.grp{display:flex;align-items:center;gap:var(--sp-2);padding:var(--sp-1) var(--sp-4);
  background:var(--bg-2);border-bottom:1px solid var(--line);
  color:var(--dim);font-size:var(--fs-2xs);font-weight:var(--fw-semi);letter-spacing:.035em}
.grp .cnt{color:var(--dim-2);font-weight:var(--fw-medium);font-variant-numeric:tabular-nums}
.cols{display:grid;grid-template-columns:repeat(auto-fit,minmax(19rem,1fr));gap:var(--sp-3);
  align-items:start}

/* ---- section headings (standalone, outside a sheet) ---- */
h2{color:var(--fg);font-size:var(--fs-sm);letter-spacing:-.005em;margin:0;
  font-weight:var(--fw-semi);display:flex;align-items:center;gap:var(--sp-3)}
h2::after{content:"";flex:1;height:1px;background:var(--line)}
h3{letter-spacing:-.005em;font-size:var(--fs-md);margin:0 0 var(--sp-2);font-weight:var(--fw-semi)}

/* ---- controls ---- */
input,.card input,.card select,.form-grid input,.form-grid select,
.settings-card select,.settings-card input:not([type=checkbox]),.mm-row input,.hdr-row input,
.tools input{
  background:var(--bg);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.42rem .6rem;font:inherit;font-size:var(--fs-sm);
  transition:border-color .12s,box-shadow .12s}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);
  box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 22%,transparent)}
input::placeholder{color:var(--dim-2)}
button{background:var(--panel-2);border:1px solid var(--line);color:var(--fg);
  border-radius:var(--r-sm);padding:.4rem .8rem;font:inherit;font-size:var(--fs-xs);
  font-weight:var(--fw-medium);letter-spacing:-.003em;cursor:pointer;
  transition:background .12s,border-color .12s;white-space:nowrap}
button:hover{background:var(--line);border-color:var(--line-2)}
button:focus-visible,.sw:focus-visible,input[type=checkbox]:focus-visible{
  outline:2px solid var(--accent);outline-offset:2px}
button:disabled{opacity:.45;cursor:not-allowed}
/* One signal-filled button per sheet marks the action that commits that sheet.
   Everything else stays a quiet hairline control. */
button.primary{background:var(--accent);border-color:var(--accent);color:var(--on-accent);
  font-weight:var(--fw-semi)}
button.primary:hover{background:color-mix(in srgb,var(--accent) 86%,#fff);border-color:var(--accent)}
#all,#none,#retry{background:transparent}
.btn-sm{padding:.26rem .6rem;font-size:var(--fs-xs)}
.btn-del{color:var(--bad);border-color:color-mix(in srgb,var(--bad) 40%,var(--line))}
.btn-del:hover{background:color-mix(in srgb,var(--bad) 14%,transparent);border-color:var(--bad)}
.test-res{font-size:var(--fs-xs);display:inline-flex;align-items:center;gap:.4rem}
.dim{color:var(--dim)}

/* ---- rows ---- */
.row{display:flex;align-items:center;gap:var(--sp-3);padding:.5rem var(--sp-4);
  background:var(--panel);cursor:pointer;transition:background .12s}
.row:hover{background:var(--panel-2)}
.row.hide{display:none}
/* .row is shared chrome: model toggles, provider rows, captcha workers. Only
   model-toggle rows carry data-id + .sw; every models-only script hook keys on
   that pair so other pages are left alone. */
.row.static{cursor:default}
.name{flex:1;display:flex;align-items:center;gap:var(--sp-2);min-width:0;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.id{color:var(--dim);font-size:var(--fs-xs);font-family:var(--mono);letter-spacing:-.01em}

/* ---- badges ---- */
/* Status is carried by a word plus a shape, not by hue alone: a dot is a second
   channel for a red-green colorblind operator. The dot is a box for states that
   are not live (killed, provider tags) and a circle for live ones. */
.badge{color:var(--bad);font-size:var(--fs-2xs);font-weight:var(--fw-medium);line-height:1.5;
  padding:0 .1rem;white-space:nowrap;display:inline-flex;align-items:center;gap:.34rem}
.badge::before{content:"";width:6px;height:6px;flex:none;border-radius:50%;
  background:currentColor}
.badge.sq::before{border-radius:1px}
.badge.ok{color:var(--on)}
.badge.warn{color:var(--warn)}
.badge.mute{color:var(--dim-2)}
/* Provider tags on a model row read as data, not as state. */
.badge.pb{color:var(--on);opacity:.9;font-family:var(--mono);font-size:var(--fs-2xs);
  border:1px solid var(--line-2);border-radius:4px;padding:0 .3rem}
.badge.pb::before{display:none}
.chip{font-family:var(--mono);font-size:var(--fs-2xs);color:var(--dim);
  border:1px solid var(--line-2);border-radius:4px;padding:.06rem .32rem;
  background:var(--bg);letter-spacing:-.01em}

/* ---- switch ---- */
.sw{appearance:none;flex:none;width:38px;height:21px;border:1px solid var(--line-2);
  border-radius:999px;background:var(--bg);position:relative;cursor:pointer;
  transition:background .15s,border-color .15s;margin:0;padding:0}
.sw:checked{background:var(--on);border-color:var(--on)}
.sw::after{content:"";position:absolute;top:2px;left:2px;width:15px;height:15px;
  border-radius:50%;background:var(--fg);transition:transform .15s;
  box-shadow:0 1px 2px rgba(0,0,0,.45)}
.sw:checked::after{transform:translateX(17px);background:var(--on-accent)}
.row:has(.sw:not(:checked)) .name{color:var(--dim);text-decoration:line-through;
  text-decoration-color:var(--dim-2)}
.del-btn{flex:none;background:none;border:1px solid transparent;color:var(--dim);
  font-size:1.05rem;padding:0 .35rem;border-radius:6px;line-height:1;cursor:pointer;
  opacity:0;transition:opacity .15s,color .15s}
.row:hover .del-btn,.del-btn:focus-visible{opacity:1}
.del-btn:hover{color:var(--bad);border-color:var(--bad);background:none}

/* ---- cards & forms ---- */
.card{display:grid;grid-template-columns:repeat(auto-fit,minmax(11rem,1fr));
  gap:var(--sp-2);padding:var(--sp-3) var(--sp-4);border:1px solid var(--line);
  border-radius:var(--r-lg);background:var(--panel);margin:0}
.card button{white-space:nowrap}
.settings-card{display:block;padding:var(--sp-4)}
.settings-card p{color:var(--dim);font-size:var(--fs-sm);margin:.1rem 0 .9rem;max-width:52rem;
  line-height:var(--lh-base)}
.settings-card p:last-child{margin-bottom:0}
.settings-card code{font-family:var(--mono);font-size:.8em;color:var(--fg);
  background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:.05rem .3rem}
.settings-card select,.settings-card input:not([type=checkbox]){display:block;
  width:min(100%,32rem);margin:.3rem 0 .8rem}
.settings-card label{color:var(--dim);font-size:var(--fs-xs);font-weight:var(--fw-medium)}
.form-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(13rem,1fr));
  gap:.6rem var(--sp-3);margin-bottom:.6rem}
.form-grid label,.form-grid input,.form-grid select{display:block;width:100%}
.form-grid label{color:var(--dim);font-size:var(--fs-xs);font-weight:var(--fw-medium);
  margin-bottom:.2rem}
.mm-row,.hdr-row{display:flex;gap:var(--sp-2);align-items:center;margin-bottom:.3rem}
.mm-row input,.hdr-row input{font-size:var(--fs-sm);min-width:0;padding:.34rem .5rem}
.mm-row input.mm-alias{flex:1.2}
.mm-row input.mm-upstream{flex:1.5}
.mm-row input.mm-fallbacks{flex:1.3}
.mm-row label{display:flex;align-items:center;gap:.25rem;color:var(--dim);
  font-size:var(--fs-xs);white-space:nowrap;margin:0 .2rem}
.hdr-row input.hdr-name{flex:1}
.hdr-row input.hdr-val{flex:1.5}
.del-row-btn{background:none;border:none;color:var(--dim);font-size:1.1rem;
  cursor:pointer;padding:0 .3rem;line-height:1}
.del-row-btn:hover{color:var(--bad);background:none}
.bar{display:flex;gap:var(--sp-2);align-items:center;flex-wrap:wrap}

/* ---- provider rows ---- */
.prov-card{background:var(--panel);padding:.7rem var(--sp-4);transition:background .12s}
.prov-card:hover{background:var(--panel-2)}
.prov-head{display:flex;align-items:center;gap:.55rem;flex-wrap:wrap}
.prov-head strong{letter-spacing:-.005em;font-weight:var(--fw-semi)}
.prov-meta{color:var(--dim-2);font-size:var(--fs-2xs);font-family:var(--mono);
  margin-left:auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  max-width:45%}
.prov-aliases{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.45rem}
.prov-actions{display:flex;gap:.35rem;align-items:center;flex-wrap:wrap;margin-top:.5rem}
.prov-card .sw{margin-left:.2rem}
.alias-lbl{color:var(--dim-2);font-size:var(--fs-2xs);font-weight:var(--fw-medium)}

/* ---- cycle list: an ordered priority list, so it is numbered ---- */
.cycle-list{border:1px solid var(--line);border-radius:var(--r-sm);margin:.3rem 0 .8rem;
  background:var(--bg);max-height:20rem;overflow:auto}
.cycle-option{display:flex;align-items:center;gap:var(--sp-2);padding:.3rem .5rem;
  border-bottom:1px solid var(--line)}
.cycle-option:last-child{border-bottom:0}
.cycle-option .pos{color:var(--dim-2);font-family:var(--mono);font-size:var(--fs-2xs);
  min-width:1.2rem;text-align:right}
.cycle-option label{display:flex;align-items:center;gap:var(--sp-2);flex:1;
  overflow-wrap:anywhere;color:var(--fg);font-size:var(--fs-sm)}
.cycle-option input[type=checkbox]{flex:none;width:.95rem;height:.95rem;margin:0;padding:0;
  accent-color:var(--accent)}
.cycle-option button{padding:.05rem .35rem;font-size:var(--fs-xs)}

/* ---- toast ---- */
#toast{position:fixed;right:1.2rem;bottom:1.2rem;padding:.55rem .85rem;
  border-radius:var(--r);border:1px solid var(--line-2);background:var(--panel-2);
  box-shadow:0 10px 30px rgba(0,0,0,.55);opacity:0;transform:translateY(6px);
  transition:opacity .18s,transform .18s;pointer-events:none;font-size:var(--fs-sm)}

/* ---- dashboard ---- */
/* A reading board, not eight identical cards: one continuous sheet divided by
   hairlines, so the numbers compare down a column instead of floating apart.
   The numbers are the hero; the tile chrome stays plain. */
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5rem,1fr));
  gap:1px;background:var(--line);border:1px solid var(--line);
  border-radius:var(--r-lg);overflow:hidden}
.tile{background:var(--panel);padding:var(--sp-3) var(--sp-4)}
.tile .lbl{color:var(--dim);font-size:var(--fs-2xs);margin-bottom:.1rem;font-weight:var(--fw-medium);
  letter-spacing:.005em}
.tile .big{font-size:var(--fs-2xl);font-weight:var(--fw-semi);line-height:var(--lh-tight);
  letter-spacing:-.03em;font-family:var(--mono);font-variant-numeric:tabular-nums;color:var(--fg)}
.tile svg{margin-top:.45rem}
.chartbox{border:1px solid var(--line);border-radius:var(--r-lg);background:var(--panel);
  padding:.7rem var(--sp-4) .5rem}
.chartrow{display:grid;grid-template-columns:repeat(auto-fit,minmax(24rem,1fr));
  gap:var(--sp-3)}
.note{color:var(--dim-2);font-size:var(--fs-xs);margin:.2rem 0;max-width:60rem;
  line-height:var(--lh-snug)}
#frames{display:flex;gap:.25rem;flex-wrap:wrap;margin-left:auto}
.grp.hide{display:none}
#frames button{padding:.15rem .5rem;font-size:var(--fs-2xs);font-weight:var(--fw-medium)}
.fp.on{background:var(--accent);border-color:var(--accent);color:var(--on-accent)}
.logwrap{max-height:22rem;overflow:auto}
.chhead{display:flex;gap:var(--sp-4);margin-bottom:.35rem;flex-wrap:wrap}
.lg{color:var(--dim);font-size:var(--fs-2xs);display:inline-flex;align-items:center;gap:.3rem}
.chwrap{position:relative}
.ax{fill:var(--dim-2);font-size:9px;font-family:var(--mono)}
/* Chart chrome draws from the same tokens as the page, so the charts are part
   of the console rather than a pasted-on widget. */
.gridline{stroke:var(--line);stroke-width:1}
.crosshair{stroke:var(--dim-2);stroke-width:1}
.dot{stroke:var(--panel);stroke-width:2}
.tip{position:absolute;pointer-events:none;background:var(--panel-2);
  border:1px solid var(--line-2);border-radius:var(--r-sm);padding:.5rem .65rem;
  min-width:120px;font-size:var(--fs-xs);box-shadow:0 10px 30px rgba(0,0,0,.55);z-index:5}
.tip.hidden{display:none}
.tip-h{color:var(--dim);margin-bottom:.3rem;font-family:var(--mono);font-size:var(--fs-2xs)}
.tip-r{display:flex;justify-content:space-between;gap:var(--sp-4);line-height:1.6}
.tip-k{color:var(--dim);display:inline-flex;align-items:center;gap:.35rem}
.tip-sw{width:10px;height:2px;border-radius:1px;display:inline-block}
.tip-r b{font-weight:var(--fw-semi);font-variant-numeric:tabular-nums}
table.stats{width:100%;border-collapse:collapse;font-size:var(--fs-xs);
  font-variant-numeric:tabular-nums}
table.stats th,table.stats td{padding:.42rem .7rem;border-bottom:1px solid var(--line);
  text-align:right;white-space:nowrap}
table.stats thead th{cursor:pointer;user-select:none;color:var(--dim);
  background:var(--panel-2);position:sticky;top:0;font-weight:var(--fw-medium);
  font-size:var(--fs-2xs)}
table.stats thead th:hover{color:var(--fg)}
table.stats thead th.on{color:var(--fg)}
table.stats thead th.on.asc::after{content:" ↑"}
table.stats thead th.on:not(.asc)::after{content:" ↓"}
table.stats td.nm{text-align:left;font-family:var(--mono);font-size:var(--fs-2xs)}
table.stats tbody tr:hover{background:var(--panel-2)}
table.stats tbody tr:last-child td{border-bottom:0}

/* ---- deleted models ---- */
#deleted-section{border:1px solid var(--line);border-radius:var(--r-lg);
  background:var(--panel);overflow:hidden}
#deleted-section.collapsed #deleted-list{display:none}
#deleted-header{display:flex;align-items:center;gap:.6rem;padding:.6rem var(--sp-4);
  cursor:pointer;user-select:none;font-size:var(--fs-sm);font-weight:var(--fw-medium)}
#deleted-header:hover{background:var(--panel-2)}
#deleted-header .cnt{color:var(--warn);font-size:var(--fs-xs);font-weight:var(--fw-semi)}
#deleted-header .arrow{color:var(--dim);font-size:.68rem;transition:transform .15s}
#deleted-section:not(.collapsed) #deleted-header .arrow{transform:rotate(90deg)}
#deleted-list{border-top:1px solid var(--line)}
.del-row{display:flex;align-items:center;gap:.6rem;padding:.4rem var(--sp-4);
  background:var(--panel)}
.del-row .id{flex:1}
.del-row button{border-color:color-mix(in srgb,var(--on) 40%,var(--line));
  color:var(--on);padding:.18rem .5rem;font-size:var(--fs-xs)}
.del-row button:hover{background:color-mix(in srgb,var(--on) 14%,transparent);border-color:var(--on)}

/* ---- narrow screens ---- */
@media (max-width:640px){
  .wrap,main{padding-left:.9rem;padding-right:.9rem}
  .head-row{gap:var(--sp-3)}
  .wordmark,.brand-sep{display:none}
  .tab-bar{display:flex;width:100%;justify-content:space-between;gap:var(--sp-2)}
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
    <div class=brand>
      <span class=brand-mark aria-hidden=true><svg width=16 height=16 viewBox="0 0 16 16"><rect x=1 y=2.5 width=14 height=2.5 rx=1.25 fill=currentColor opacity=.95 /><rect x=1 y=6.75 width=10 height=2.5 rx=1.25 fill=currentColor opacity=.6 /><rect x=1 y=11 width=5 height=2.5 rx=1.25 fill=currentColor opacity=.32 /></svg></span>
      <span class=wordmark>gateway admin</span>
      <span class=brand-sep aria-hidden=true></span>
      <h1>` + title + `</h1>
    </div>
    <nav class=tab-bar aria-label="Admin sections">` + adminHeadTabs(active) + `</nav>
  </div>
  ` + adminHeadSub(active) + `
</div></header>
`
}
