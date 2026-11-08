package main

import (
	"html/template"
	"net/http"
)

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never let the browser cache these pages - a stale copy means old JS runs
	// against a new server (which is exactly how the signup page got "stuck").
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_ = t.ExecuteTemplate(w, "layout", data)
}

func page(body string) *template.Template {
	return template.Must(template.New("p").Parse(layout + body))
}

var (
	homeTmpl    = page(homeBody)
	loginTmpl   = page(loginBody)
	signupTmpl  = page(signupBody)
	accountTmpl = page(accountBody)
	forgotTmpl  = page(forgotBody)
	resetTmpl   = page(resetBody)
	// admin templates (Users + Broadcast pages) live in admin_page.go
)

// --- shared layout + design system --------------------------------------

const layout = `{{define "layout"}}<!DOCTYPE html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Clusterheart</title>
<link rel="icon" href="https://clusterheart.com/favicon.ico" sizes="48x48"><link rel="icon" type="image/svg+xml" href="https://clusterheart.com/favicon.svg"><link rel="apple-touch-icon" href="https://clusterheart.com/apple-touch-icon.png">
<script>(function(){var t='dark',a='green';try{t=localStorage.getItem('ch-theme')||'dark';a=localStorage.getItem('ch-accent')||'green';}catch(e){}var c=document.documentElement.classList;if(t==='dark'){c.add('dark');if(['blue','purple','cyan','coral'].indexOf(a)>-1)c.add(a);}})();</script>
<style>
@import url('https://fonts.googleapis.com/css2?family=Geist:wght@300..800&family=Geist+Mono:wght@400..700&display=swap');
:root{
  --bg:#ffffff;--panel:#ffffff;--surface2:#fafafa;--input-bg:#ffffff;
  --text:#0a0a0a;--muted:#525252;--faint:#a3a3a3;
  --border:#e5e5e5;--border-strong:#d4d4d4;
  --accent:#0a0a0a;--accent2:#262626;--accent-ink:#ffffff;
  --active-bg:#ffffff;--hover:rgba(0,0,0,.05);--focus:rgba(0,0,0,.09);
  --green:#16a34a;--amber:#d97706;--red:#dc2626;
  /* chart series, fixed order (validated for colour-blind separation) */
  --s1:#2a78d6;--s2:#eb6834;--s3:#1baf7a;--s4:#eda100;--s5:#e87ba4;--s6:#008300;--s7:#6250d6;--s8:#e34948;
}
.dark{
  --bg:#070809;--panel:#070809;--surface2:#0b0c0d;--input-bg:#101113;
  --text:#E8EBEE;--muted:#9CA1A8;--faint:#767B82;
  --border:#222528;--border-strong:#32363A;
  --accent:#00E464;--accent2:#46F08C;--accent-ink:#03120A;
  --active-bg:#17191B;--hover:color-mix(in srgb,var(--accent) 8%,transparent);--focus:color-mix(in srgb,var(--accent) 18%,transparent);
  --green:#4ade80;--amber:#fb923c;--red:#f87171;
  --s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--s5:#d55181;--s6:#008300;--s7:#9085e9;--s8:#e66767;
}
/* accent schemes, same neutrals (match clusterheart.com) */
.dark.blue{--accent:#8EC6FA;--accent2:#B2D8FC;--accent-ink:#05101E}
.dark.purple{--accent:#A259FF;--accent2:#B880FF;--accent-ink:#ffffff}
.dark.cyan{--accent:#1ABCFE;--accent2:#5ACDFF;--accent-ink:#02121C}
.dark.coral{--accent:#FF7262;--accent2:#FF9182;--accent-ink:#1E0805}
.logo-mark{color:var(--accent)}
.logo-mark{transform-box:fill-box;transform-origin:center;cursor:pointer}
[data-theme-logo]{cursor:pointer}
body,body *{transition:background-color .3s ease,border-color .3s ease,color .3s ease}
*{box-sizing:border-box}
html{scroll-behavior:smooth}
html,body{margin:0;min-height:100%}
#download{scroll-margin-top:68px}
body{
  background:var(--bg);color:var(--text);
  font-family:'Geist',ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  -webkit-font-smoothing:antialiased;line-height:1.5;
}
::selection{background:var(--accent);color:var(--accent-ink)}
h1,h2,h3{text-wrap:balance}p,li{text-wrap:pretty}
.mono{font-family:'Geist Mono',ui-monospace,SFMono-Regular,Menlo,monospace}
a{color:var(--text);text-decoration:none;font-weight:600}
a:hover{color:var(--accent)}
.center{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:32px;background:var(--bg)}
.card{
  width:100%;max-width:410px;padding:46px 42px;
  background:var(--panel);
}
.brand{display:flex;align-items:center;gap:10px;margin-bottom:34px}
.wordmark{font-size:18px;font-weight:700;letter-spacing:-.5px;color:var(--text)}
/* auth pages: big logo on the left, form on the right (in-card brand hidden) */
.card .brand{display:none}
.auth-split{display:flex;min-height:100vh}
.auth-left{flex:1;display:flex;align-items:center;justify-content:center;background:var(--surface2);border-right:1px solid var(--border);padding:40px}
.auth-right{flex:1;display:flex;align-items:center;justify-content:center;padding:40px}
@media(max-width:820px){.auth-left{display:none}}
.wordmark .a{color:inherit}
.kicker{font-family:'Geist Mono',ui-monospace,monospace;font-size:11px;font-weight:500;letter-spacing:.2em;text-transform:uppercase;color:var(--faint);margin:0 0 14px}
h1{font-size:30px;font-weight:800;letter-spacing:-1px;line-height:1.08;margin:0 0 9px;color:var(--text)}
.sub{color:var(--muted);font-size:14px;margin:0 0 30px}
label{display:block;font-size:13px;font-weight:500;color:var(--text);margin:18px 0 8px}
input{
  width:100%;padding:12px 14px;font-size:14px;color:var(--text);font-family:inherit;
  background:var(--input-bg);border:1px solid var(--border-strong);border-radius:0;outline:none;
  transition:border-color .15s,box-shadow .15s;
}
input:focus{border-color:var(--accent);box-shadow:0 0 0 3px var(--focus)}
input::placeholder{color:var(--faint)}
.pw-wrap{position:relative}
.pw-wrap input{padding-right:46px}
.pw-eye{position:absolute;right:6px;top:50%;transform:translateY(-50%);width:34px;height:34px;display:flex;align-items:center;justify-content:center;background:none;border:none;cursor:pointer;color:var(--faint);padding:0}
.pw-eye:hover{color:var(--muted)}
.pw-eye .eye-off{display:none}
.pw-eye.on .eye{display:none}
.pw-eye.on .eye-off{display:block}
.btn{
  width:100%;margin-top:28px;padding:14px 16px;font-size:14px;font-weight:600;color:var(--accent-ink);cursor:pointer;font-family:inherit;
  background:var(--accent);border:1px solid var(--accent);border-radius:0;
  transition:background .15s,opacity .12s;
}
.btn:hover{background:var(--accent2)}
.btn:active{background:var(--accent)}
.btn:disabled{opacity:.45;cursor:default}
.btn.secondary{margin-top:12px;background:transparent;color:var(--text);border:1px solid var(--border-strong)}
.btn.secondary:hover{background:var(--hover);border-color:var(--accent)}
.alt{margin-top:24px;font-size:13.5px;color:var(--muted);text-align:center}
.alt a{color:var(--accent);font-weight:600}
.trust{margin-top:24px;padding-top:22px;border-top:1px solid var(--border);display:flex;align-items:center;justify-content:center;gap:8px;font-size:12.5px;color:var(--faint)}
.msg{min-height:20px;margin-top:16px;font-size:13px}
.msg.err{color:var(--red)}.msg.ok{color:var(--green)}.msg.info{color:var(--muted)}

/* --- admin (sidebar shell, Apple-style) --- */
.admin-shell{display:flex;min-height:100vh}
.admin-nav{width:238px;flex-shrink:0;background:var(--surface2);border-right:1px solid var(--border);padding:22px 14px;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow:hidden;white-space:nowrap;transition:width .28s cubic-bezier(.4,0,.2,1),padding .28s cubic-bezier(.4,0,.2,1),opacity .2s ease}
.an-brand{display:flex;flex-direction:row;align-items:center;justify-content:space-between;gap:10px;padding:2px 14px 14px;margin:0 -14px 12px;border-bottom:1px solid var(--border)}
.an-brandrow{display:flex;align-items:center;gap:9px;min-width:0}
.an-brand .wordmark{font-size:16px}
.an-list{display:flex;flex-direction:column;gap:2px}
.nav-item{display:block;padding:9px 12px;border-radius:0;font-size:14px;font-weight:500;color:var(--muted)}
.nav-item:hover{background:var(--hover);color:var(--text)}
.nav-item.active{background:var(--active-bg);color:var(--text);font-weight:600;box-shadow:0 0 0 1px var(--border)}
.an-foot{margin-top:auto;padding:14px 10px 4px;font-size:12.5px;color:var(--muted)}
.an-me{font-weight:500;color:var(--text);margin-bottom:3px}
.an-logout{color:var(--muted)}.an-logout:hover{color:var(--accent)}
.admin-main{flex:1 1 auto;min-width:0;padding:32px 40px;position:relative;transition:padding .28s cubic-bezier(.4,0,.2,1)}
@media(max-width:720px){
  .admin-shell{flex-direction:column}
  .admin-nav{width:100%;height:auto;position:static;flex-direction:row;align-items:center;border-right:none;border-bottom:1px solid var(--border);padding:12px 16px}
  .an-brand{padding:0;margin-right:16px}
  .an-list{flex-direction:row}
  .an-foot{margin:0 0 0 auto;padding:0}
  .admin-main{padding:32px 20px}
}
.count{display:inline-flex;align-items:center;justify-content:center;min-width:20px;height:20px;padding:0 7px;margin-right:7px;border-radius:999px;background:var(--accent);color:var(--accent-ink);font-size:11.5px;font-weight:700;vertical-align:middle}

/* cards */
.card2{background:var(--surface2);border:1px solid var(--border);border-radius:0;overflow:hidden;margin:0 0 20px}
.card2-head{padding:20px 22px 16px;border-bottom:1px solid var(--border)}
.card2-title{font-size:15px;font-weight:650;letter-spacing:-.2px;color:var(--text);margin:0}
.card2-desc{font-size:13px;color:var(--muted);margin:3px 0 0}
.card2-body{padding:18px 22px 20px}
/* scrollable table area with a pinned header (for long pod/job lists) */
.tscroll{max-height:440px;overflow:auto;border-top:1px solid var(--border)}
.tscroll table.users th{position:sticky;top:0;background:var(--input-bg);z-index:1}
/* --- app dashboard: clean top bar + burger menu --- */
.app2{min-height:100vh;display:flex;flex-direction:column}
.topbar{position:sticky;top:0;z-index:50;display:flex;align-items:center;justify-content:space-between;gap:16px;padding:14px 28px;background:color-mix(in srgb,var(--bg) 80%,transparent);backdrop-filter:saturate(180%) blur(14px);-webkit-backdrop-filter:saturate(180%) blur(14px);border-bottom:1px solid var(--border)}
.tb-brand{display:flex;align-items:center;gap:10px}
.tb-brand:hover{color:var(--text)}
.tb-brand .wordmark{font-size:16px;font-weight:700;letter-spacing:-.3px;color:var(--text)}
.tb-right{display:flex;align-items:center;gap:12px}
.tb-add{padding:9px 16px;font-size:13px;white-space:nowrap}
.menu{position:relative}
.menu-btn{display:inline-flex;align-items:center;justify-content:center;width:40px;height:40px;border-radius:10px;background:var(--surface2);border:1px solid var(--border);color:var(--text);cursor:pointer;transition:background .14s,border-color .14s}
.menu-btn:hover{background:var(--hover);border-color:var(--border-strong)}
.menu-pop{position:absolute;top:calc(100% + 10px);right:0;min-width:214px;background:var(--panel);border:1px solid var(--border-strong);border-radius:13px;box-shadow:0 20px 52px -16px rgba(0,0,0,.42),0 2px 8px -2px rgba(0,0,0,.14);padding:6px;opacity:0;transform:translateY(-6px) scale(.98);transform-origin:top right;pointer-events:none;transition:opacity .16s ease,transform .18s cubic-bezier(.4,0,.2,1);z-index:60}
.menu.open .menu-pop{opacity:1;transform:none;pointer-events:auto}
.menu-email{padding:7px 12px 10px;font-size:12px;color:var(--faint);border-bottom:1px solid var(--border);margin:0 0 4px;word-break:break-all}
.menu-item{display:block;width:100%;text-align:left;padding:9px 12px;border:none;background:none;border-radius:8px;font-family:inherit;font-size:14px;font-weight:500;color:var(--text);cursor:pointer}
.menu-item:hover{background:var(--hover);color:var(--text)}
.menu-item.active{font-weight:650}
.menu-item.accent{color:var(--accent);font-weight:650}
.menu-item.accent:hover{background:var(--hover);color:var(--accent)}
.menu-sep{height:1px;background:var(--border);margin:4px 6px}
.menu-item.danger{color:var(--red)}
.menu-item.danger:hover{background:color-mix(in srgb,var(--red) 12%,transparent)}
.app2-main{flex:1 1 auto;width:100%;max-width:1080px;margin:0 auto;padding:30px 28px 64px}
@media(max-width:640px){.topbar{padding:12px 16px}.app2-main{padding:22px 16px 48px}.tb-add{padding:8px 13px}}
/* cluster list: compact responsive card grid (no horizontal scroll) */
.cl-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(270px,1fr));gap:14px}
.cl-card{display:flex;flex-direction:column;gap:14px;padding:16px 18px;background:var(--surface2);border:1px solid var(--border);transition:border-color .14s,background .14s}
.cl-card:hover{border-color:var(--border-strong);background:var(--hover)}
.cl-grid .cl-card{cursor:grab;user-select:none;-webkit-user-select:none;-webkit-touch-callout:none;position:relative;scale:1;transition:border-color .14s,background .14s,scale .2s cubic-bezier(.2,.8,.2,1),box-shadow .2s}
.cl-grid.arranging,.cl-grid.arranging *{cursor:grabbing}
.cl-grid.arranging .cl-card{will-change:transform}
.cl-card.lifted{z-index:20;pointer-events:none;scale:1.035;border-color:var(--accent);background:var(--panel);box-shadow:0 26px 56px -20px rgba(0,0,0,.65),0 0 0 1px color-mix(in srgb,var(--accent) 35%,transparent)}
.cl-card.settling{z-index:20}
@media (prefers-reduced-motion:reduce){.cl-grid .cl-card,.cl-card.settling{transition:none!important}}
.cl-head{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}
.cl-name{font-size:16px;font-weight:700;letter-spacing:-.3px;color:var(--text);line-height:1.2;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;display:block}
.cl-name:hover{color:var(--accent)}
.cl-env{margin-top:4px;font-size:10.5px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--faint)}
.cl-status{display:inline-flex;align-items:center;gap:6px;font-size:12.5px;font-weight:600;white-space:nowrap;flex-shrink:0}
.cl-status.on{color:var(--green)}
.cl-status.off{color:var(--amber)}
.cl-status .dot{width:7px;height:7px;border-radius:50%;background:currentColor;display:inline-block}
.cl-metrics{display:flex;gap:26px}
.cl-metric{display:flex;flex-direction:column;gap:1px}
.cl-metric b{font-size:18px;font-weight:700;color:var(--text);line-height:1.1}
.cl-metric span{font-size:10.5px;color:var(--faint);letter-spacing:.03em}
.cl-foot{display:flex;align-items:center;justify-content:space-between;gap:10px;padding-top:12px;border-top:1px solid var(--border)}
.cl-seen{font-size:12px;color:var(--muted)}
.cl-upd{color:var(--amber);font-weight:600}
.cl-foot-actions{display:flex;gap:6px;flex-shrink:0}
.cl-foot-actions .act{margin:0;padding:6px 12px;font-size:12.5px}
/* loading spinner (accent comet ring) */
.spin-wrap{display:flex;justify-content:center;align-items:center;padding:60px 0}
.spinner{width:36px;height:36px;border-radius:50%;border:3px solid var(--border-strong);border-top-color:var(--accent);animation:ch-spin .75s linear infinite}
@keyframes ch-spin{to{transform:rotate(1turn)}}
/* red "failed" badge shown on Pods/Jobs labels when there are failures */
.fail-badge{display:inline-block;margin-left:6px;padding:1px 6px;border-radius:999px;background:color-mix(in srgb,var(--red) 16%,transparent);color:var(--red);font-size:10px;font-weight:700;letter-spacing:.01em;vertical-align:middle;white-space:nowrap}
.seg{display:inline-flex;height:36px;border:1px solid var(--border-strong);overflow:hidden;flex-shrink:0}
.seg-btn{display:inline-flex;align-items:center;height:100%;padding:0 15px;font-size:12.5px;font-weight:600;background:transparent;color:var(--muted);border:none;cursor:pointer;font-family:inherit;white-space:nowrap;transition:background .14s,color .14s}
.seg-btn:hover:not(.active){color:var(--text)}
.seg-btn.active{background:var(--accent);color:var(--accent-ink)}
.seg-btn+.seg-btn{border-left:1px solid var(--border-strong)}
.toolbar-input{height:36px;width:auto;padding:0 12px;font-size:13px;background:var(--input-bg);border:1px solid var(--border-strong);color:var(--text);font-family:inherit;outline:none;appearance:none;-webkit-appearance:none}
.toolbar-input:focus{border-color:var(--accent)}
.toolbar-row{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.toolbar-row .seg{flex-shrink:0}
.toolbar-row .toolbar-ns{flex-shrink:0;margin-left:auto}
.toolbar-row .toolbar-search{order:3;flex:1 1 100%;width:100%}
/* four-up stat grid (desktop) -> two-up on phones */
.statgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin:16px 0}
.statcard{display:flex;flex-direction:column;padding:12px 13px;background:var(--surface2);border:1px solid var(--border);min-width:0;overflow:hidden}
.stat-val,.stat-label,.stat-sub{overflow:hidden;text-overflow:ellipsis}
.stat-label{font-size:11px;color:var(--faint);line-height:1.3;min-height:2.2em}
.stat-val{font-size:18px;font-weight:700;color:var(--text);margin-top:2px;white-space:nowrap}
.stat-sub{font-size:10.5px;color:var(--faint);margin-top:auto;padding-top:4px}
.admin-main h1{font-size:22px;letter-spacing:-.5px}
.detail-head{display:flex;align-items:center;justify-content:space-between;gap:14px;flex-wrap:wrap;margin-bottom:16px}
/* three-dot (kebab) options menu — cards + detail header */
.kebab{position:relative;display:inline-flex}
.kebab-btn{display:inline-flex;align-items:center;justify-content:center;width:34px;height:34px;border-radius:8px;background:transparent;border:1px solid transparent;color:var(--muted);cursor:pointer;transition:background .14s,color .14s,border-color .14s}
.kebab-btn:hover{background:var(--hover);color:var(--text)}
.kebab.open .kebab-btn{background:var(--hover);color:var(--text);border-color:var(--border)}
.kebab-pop{position:absolute;top:calc(100% + 6px);right:0;min-width:170px;background:var(--panel);border:1px solid var(--border-strong);border-radius:11px;box-shadow:0 16px 44px -14px rgba(0,0,0,.5),0 2px 8px -2px rgba(0,0,0,.2);padding:5px;opacity:0;transform:translateY(-6px) scale(.98);transform-origin:top right;pointer-events:none;transition:opacity .14s ease,transform .16s cubic-bezier(.4,0,.2,1);z-index:60}
.kebab.open .kebab-pop{opacity:1;transform:none;pointer-events:auto}
.kebab-item{display:block;width:100%;text-align:left;padding:8px 11px;border:none;background:none;border-radius:7px;font-family:inherit;font-size:13.5px;font-weight:500;color:var(--text);cursor:pointer;white-space:nowrap}
.kebab-item:hover{background:var(--hover)}
.kebab-item.danger{color:var(--red)}
.kebab-item.danger:hover{background:color-mix(in srgb,var(--red) 12%,transparent)}
.kebab-sep{height:1px;background:var(--border);margin:4px 6px}
.rename-name{cursor:default;border-radius:4px}
.rename-name:hover{text-decoration:underline;text-decoration-color:var(--border-strong);text-decoration-thickness:1px;text-underline-offset:4px}
.actbar{display:flex;gap:6px;flex-wrap:wrap}
.actbar .act{padding:6px 12px;font-size:12.5px}
/* apple-style small modal */
.modal-bg{position:fixed;inset:0;background:rgba(0,0,0,.55);backdrop-filter:blur(3px);display:flex;align-items:center;justify-content:center;z-index:200;padding:20px;animation:mfade .15s ease}
@keyframes mfade{from{opacity:0}to{opacity:1}}
.modal{width:100%;max-width:330px;background:var(--surface2);border:1px solid var(--border-strong);border-radius:14px;padding:20px;text-align:center;box-shadow:0 24px 64px -18px rgba(0,0,0,.85)}
.modal h3{margin:0 0 6px;font-size:16px;font-weight:650;color:var(--text)}
.modal p{margin:0 0 14px;font-size:13px;color:var(--muted);line-height:1.5}
.modal input{width:100%;margin:2px 0 14px;font-size:14px;padding:9px 11px}
.modal-actions{display:flex;gap:8px}
.modal-actions button{flex:1 1 auto;padding:9px 14px;font-size:13.5px;font-weight:600;font-family:inherit;cursor:pointer;white-space:nowrap;border:1px solid var(--border-strong);background:transparent;color:var(--text);border-radius:8px}
.modal-actions .primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink)}
.modal-actions .danger{background:#b91c1c;border-color:#b91c1c;color:#fff}
.modal-actions button:disabled{opacity:.4;cursor:default}
@media(max-width:720px){
  .admin-main{padding:18px 14px}
  .admin-main h1{font-size:19px}
  .card2{overflow-x:auto}
  .card2-body,.card2-head{padding-left:14px;padding-right:14px}
  table.users th,table.users td{padding:10px 12px;font-size:12.5px}
  .tscroll{max-height:62vh}
  .an-me{display:none}
  .admin-nav{overflow:visible;white-space:normal;flex-wrap:wrap;gap:10px 14px;padding:12px 14px}
  .an-brand{border-bottom:none;margin:0;padding:0;justify-content:flex-start}
  .an-list{gap:16px;flex-wrap:wrap}
  .statgrid{gap:6px}
  .statcard{padding:9px 8px}
  .stat-label{font-size:9.5px}
  .stat-val{font-size:16px}
  .stat-sub{display:none}
  /* app tables keep real columns + headers (scroll sideways) instead of the
     admin-style label stacking, which has no labels here */
  #clusters table.users{display:table;width:100%;min-width:600px}
  #clusters table.users thead{display:table-header-group}
  #clusters table.users tbody{display:table-row-group}
  #clusters table.users tr{display:table-row}
  #clusters table.users th,#clusters table.users td{display:table-cell;width:auto}
  #clusters table.users td::before{display:none}
}

/* broadcast notice composer */
textarea.nb{width:100%;padding:13px 14px;font-family:inherit;font-size:14px;color:var(--text);background:var(--input-bg);border:1px solid var(--border);border-radius:0;outline:none;resize:vertical;transition:background .15s,border-color .15s,box-shadow .15s}
textarea.nb:focus{background:var(--input-bg);border-color:var(--accent);box-shadow:0 0 0 3px var(--focus)}
.nb-actions{display:flex;align-items:center;gap:10px;margin-top:14px}
.nb-msg{font-size:13px;color:var(--muted)}

/* buttons (rounded, soft) */
.act{padding:9px 16px;border-radius:0;font-size:13px;font-weight:600;cursor:pointer;font-family:inherit;border:1px solid transparent;transition:background .14s,color .14s}
.act:disabled{opacity:.45;cursor:default}
.act.approve{color:var(--accent-ink);background:var(--accent)}
.act.approve:hover{background:var(--accent2)}
.act.forceout{background:var(--active-bg);color:var(--text);border-color:var(--border);margin-right:8px}
.act.forceout:hover{background:var(--hover)}
.act.revoke{background:var(--active-bg);color:var(--text);border-color:var(--border)}
.act.revoke:hover{background:rgba(248,113,113,.14);color:var(--red);border-color:rgba(248,113,113,.4)}

/* users table */
table.users{width:100%;border-collapse:collapse}
table.users th{text-align:left;padding:13px 22px;font-size:11px;letter-spacing:.03em;color:var(--faint);font-weight:600;background:var(--input-bg);border-bottom:1px solid var(--border)}
table.users td{padding:15px 22px;vertical-align:middle}
table.users tbody tr{border-top:1px solid var(--border);transition:background .12s}
table.users tbody tr:first-child{border-top:none}
table.users tbody tr:hover{background:var(--hover)}
.u-name{font-weight:600;font-size:14.5px;color:var(--text)}
.pod-name{font-size:13.5px;max-width:260px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
/* node utilization: colour-coded usage number (no bar) */
.util-val{font-size:13px;font-weight:600;white-space:nowrap}
.util-val.ok{color:var(--green)}
.util-val.warn{color:var(--amber)}
.util-val.crit{color:var(--red)}
.util-val.cap{color:var(--muted);font-weight:500}
/* detail tabs (Pods / Nodes) */
/* Latency check */
.lat-form{display:flex;flex-wrap:wrap;align-items:flex-end;gap:12px}
.lat-f{display:flex;flex-direction:column;gap:6px;margin:0;font-size:12px;font-weight:600;color:var(--muted)}
.lat-f > span{letter-spacing:.02em}
.lat-f .toolbar-input{height:36px}
.lat-pod{flex:1 1 260px;min-width:0}.lat-pod select{width:100%}
.lat-port{width:96px}.lat-path{flex:1 1 180px}.lat-path input,.lat-port input{width:100%}
.lat-n{width:96px}
.lat-run{height:36px}
.lat-status{margin:12px 0 0}
.lat-head{flex-wrap:wrap}.lat-head .tl-sum{white-space:normal;overflow-wrap:anywhere}
.lat-stats{margin:0 0 6px}
.lat-verdict{margin:10px 0 18px;font-size:13.5px;color:var(--text)}
.lat-legend{display:flex;flex-wrap:wrap;gap:6px 18px;font-size:12.5px;color:var(--muted);margin-bottom:10px}
.lat-legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:6px;vertical-align:-1px}
.lat-bars{display:flex;flex-direction:column;gap:8px}
.lat-row{display:grid;grid-template-columns:76px minmax(0,1fr) 72px;align-items:center;gap:10px}
.lat-lbl,.lat-val{font-size:12px;color:var(--muted);font-variant-numeric:tabular-nums;white-space:nowrap}
.lat-val{text-align:right;color:var(--text)}
.lat-track{display:flex;gap:2px;height:14px;min-width:0}
.lat-seg{display:block;height:100%;min-width:2px}
.lat-seg:last-child{border-radius:0 4px 4px 0}
.lat-err{grid-column:2/4;font-size:12.5px;color:var(--red)}
@media (max-width:640px){.lat-row{grid-template-columns:58px minmax(0,1fr) 60px}.lat-port,.lat-n{width:84px}}
/* Server timeline */
.tl-bar{display:flex;align-items:center;justify-content:space-between;gap:10px;margin:0 0 14px;flex-wrap:wrap}
.tl-view{height:36px;font:inherit;font-size:12.5px;font-weight:600;color:var(--text);background:var(--input-bg);border:1px solid var(--border-strong);padding:0 15px;cursor:pointer}
.tl-view:hover{background:var(--hover)}
.subtabs .subpane[hidden]{display:none}
.subtabs .seg{max-width:100%;overflow-x:auto;scrollbar-width:none}
.subtabs .seg::-webkit-scrollbar{display:none}
.subtabs > .subpane > .tscroll{border-top:none}
.tl-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}
.tl-grid .card2{margin:0}
.tl-loading .tl-grid,.tl-loading .tl-tscroll{opacity:.5;transition:opacity .15s}
.tl-head{display:flex;align-items:baseline;justify-content:space-between;gap:10px;padding:14px 16px 10px;border-bottom:none}
.tl-title-sub{font-weight:500;color:var(--faint);font-size:12.5px}
.tl-sum{margin:0;font-size:12.5px;color:var(--muted);white-space:nowrap}
.tl-sum b{color:var(--text);font-weight:650}
.tl-chart{position:relative;padding:0;outline:none;touch-action:pan-y;line-height:0}
.tl-chart:focus-visible{box-shadow:inset 0 0 0 2px var(--focus)}
.tl-chart svg{display:block}
.tl-chart .tl-gl{stroke:var(--border);stroke-width:1;shape-rendering:crispEdges}
.tl-chart .tl-base{stroke:var(--border-strong)}
.tl-chart .tl-ax{fill:var(--faint);font-size:11px;font-variant-numeric:tabular-nums;font-family:inherit}
.tl-chart .tl-cross{stroke:var(--muted);stroke-width:1;shape-rendering:crispEdges}
.tl-chart .tl-hdot,.tl-chart .tl-end{stroke:var(--surface2);stroke-width:2}
.tl-tip{position:absolute;top:8px;z-index:3;min-width:130px;padding:8px 10px;background:var(--bg);border:1px solid var(--border-strong);box-shadow:0 6px 20px rgba(0,0,0,.18);font-size:12.5px;line-height:1.45;pointer-events:none}
.tl-tt{color:var(--muted);font-size:11.5px;margin-bottom:3px;white-space:nowrap}
.tl-row{white-space:nowrap;color:var(--text)}
.tl-row b{font-weight:650}
.tl-sn{color:var(--muted)}
.tl-pk{color:var(--faint)}
.tl-bar-r{display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.tl-hint,.tl-zoomed{font-size:12px;color:var(--faint)}
.tl-zoomed{color:var(--muted)}
.tl-chart{cursor:crosshair;user-select:none;-webkit-user-select:none}
.tl-chart .tl-sel{fill:var(--focus);fill-opacity:.15;stroke:var(--focus);stroke-opacity:.6;stroke-width:1}
.tl-pkzoom{font:inherit;font-size:12px;color:var(--accent,var(--muted));background:none;border:none;padding:0;margin-left:4px;cursor:pointer;text-decoration:underline;text-underline-offset:2px}
.tl-pkzoom:hover{color:var(--text)}
.tl-pkat{font-size:12px;color:var(--faint);margin-left:4px}
.tl-key{display:inline-block;width:12px;height:2px;border-radius:1px;vertical-align:middle;margin-right:6px}
.tl-legend{display:flex;flex-wrap:wrap;gap:6px 16px;padding:8px 16px 14px;font-size:12.5px;color:var(--muted)}
.tl-li{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:100%}
.tl-li b{color:var(--text);font-weight:650}
.tl-li-sub{color:var(--faint)}
.tl-num{font-variant-numeric:tabular-nums;white-space:nowrap}
@media (max-width:900px){.tl-grid{grid-template-columns:minmax(0,1fr)}}
.tabs{display:flex;gap:2px;border-bottom:1px solid var(--border);margin:4px 0 18px;overflow-x:auto;scrollbar-width:none}
.tabs::-webkit-scrollbar{display:none}
.ai-log{max-height:460px;overflow-y:auto;display:flex;flex-direction:column;gap:10px}
.ai-q{align-self:flex-end;max-width:80%;padding:9px 13px;font-size:13.5px;background:var(--active-bg);border:1px solid var(--border)}
.ai-a{padding:4px 0}
.ai-clear-row{display:flex;justify-content:flex-end}
.ai-clear{background:none;border:none;color:var(--muted);font:inherit;font-size:12px;cursor:pointer;padding:0}
.ai-clear:hover{color:var(--text)}
.ai-line{font-size:13.5px;line-height:1.6;padding-left:16px;position:relative}
.ai-line:before{content:"";position:absolute;left:3px;top:.62em;width:6px;height:6px;border-radius:50%;background:var(--muted)}
.ai-ok:before{background:var(--green)}
.ai-bad{color:var(--red)}.ai-bad:before{background:var(--red)}
.ai-warn{color:var(--amber)}.ai-warn:before{background:var(--amber)}
.ai-line code{font-size:12.5px;padding:1px 5px;background:var(--active-bg);border:1px solid var(--border)}
.tab-badge{display:inline-flex;align-items:center;justify-content:center;min-width:18px;height:18px;padding:0 5px;font-size:11px;font-weight:700;border-radius:9px;background:rgba(248,113,113,.16);color:var(--red)}
.tab{flex-shrink:0;white-space:nowrap;display:inline-flex;align-items:center;gap:7px;padding:9px 15px;font-size:13.5px;font-weight:600;color:var(--muted);background:none;border:none;border-bottom:2px solid transparent;margin-bottom:-1px;cursor:pointer;font-family:inherit;transition:color .14s}
.tab:hover{color:var(--text)}
.tab.active{color:var(--text);border-bottom-color:var(--accent)}
.tabpanel{display:none}
.tabpanel.active{display:block}
/* table flush to the card top when there's no header above it (nodes) */
.card2>.tscroll:first-child{border-top:none}
/* app data tables (pods/problems/warnings): consistent header+body padding so
   columns line up, an identity column that hugs its content with a divider
   after it, and single-line truncation for long values. id-scoped so it wins
   over the shared .users mobile "stacked card" rules that otherwise zero out
   body-cell padding. */
#clusters table.ptable th,#clusters table.ptable td{padding:12px 16px;vertical-align:middle}
#clusters table.ptable th:first-child,#clusters table.ptable td:first-child{width:1%;white-space:nowrap;padding-left:18px;padding-right:26px;border-right:1px solid var(--border)}
#clusters table.ptable tbody tr{border-bottom:none}
.ptable td.u-email,.ptable td.u-time{max-width:320px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.u-email{font-family:'Geist Mono',ui-monospace,SFMono-Regular,monospace;font-size:12px;color:var(--muted);margin-top:2px}
.u-time{font-size:13px;color:var(--text);white-space:nowrap}
.u-ver{font-family:'Geist Mono',ui-monospace,monospace;font-size:12.5px;color:var(--muted)}
.cell-act{text-align:right;white-space:nowrap}
.pill{display:inline-flex;align-items:center;padding:0;font-size:13px;font-weight:600;background:none}
.pill.on{color:var(--green)}
.pill.off{color:var(--amber)}
.pill.bad{color:var(--red)}
.pill.mgd{color:var(--muted)}
.rs-table th,.rs-table td{white-space:nowrap}
.rs-table td{font-size:13px}
.rs-table .rs-num{text-align:right;font-variant-numeric:tabular-nums}
.rs-table .rs-act{text-align:right;width:1%;padding-left:8px}
.rs-table .rs-act .act{padding:5px 12px;font-size:12px}
.rs-dim{color:var(--muted);font-weight:400}
.rs-sub{font-size:11.5px;color:var(--muted);margin-top:2px}
.rs-sub.rs-bad{color:var(--red)}
.rs-sub.rs-low{color:var(--amber)}
.log-box{border:1px solid var(--border);margin-bottom:10px}
.log-box summary{cursor:pointer;padding:10px 14px;list-style:none}
.log-box summary::-webkit-details-marker{display:none}
.log-box[open] summary{border-bottom:1px solid var(--border)}
.log-pre{margin:0;padding:12px 14px;max-height:320px;overflow:auto;font-size:12px;line-height:1.55;background:var(--input-bg);white-space:pre-wrap;word-break:break-word}
.sec-table td{vertical-align:top}
.sec-scroll{max-height:none}
.health-scroll{max-height:360px}
.sec-detail[hidden]{display:none !important}
#clusters table.users.sec-table{min-width:0}
.sec-box .u-email{white-space:normal}
.sec-sev-inline{display:none}
.sec-row{cursor:pointer}
.sec-row.open{background:var(--hover)}
.sec-caret{color:var(--muted);font-size:18px;width:1%;transition:transform .15s}
.sec-row.open .sec-caret{transform:rotate(90deg)}
.sec-detail td{background:var(--input-bg);padding:0 !important}
#clusters table.ptable.sec-table td:first-child{white-space:normal}
#clusters table.ptable.sec-table .sec-detail td{width:auto;max-width:0;border-right:none}
.sec-box{padding:18px 22px}
.sec-grid{display:grid;grid-template-columns:110px 1fr;gap:10px 16px;align-items:baseline}
.sec-k{font-size:11.5px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
.sec-v{font-size:13px;line-height:1.6}
.sec-mark{background:rgba(248,113,113,.18);color:var(--red);padding:0 2px}
.sec-masked{color:var(--red);border-color:rgba(248,113,113,.4)}
.sec-small{padding:3px 10px !important;font-size:12px !important;margin-left:6px}
.sec-pre{margin-top:6px;border:1px solid var(--border);max-height:280px}
.sec-steps{margin:6px 0 0 18px;padding:0;font-size:13px;line-height:1.7}
@media(max-width:640px){
#clusters table.users.sec-table,#clusters table.users.sec-table tbody{display:block;min-width:0;width:100%}
#clusters table.users.sec-table thead{display:none}
#clusters table.users.sec-table tr.sec-row{display:grid;grid-template-columns:1fr 20px;gap:8px 10px;padding:14px 14px 12px}
#clusters table.users.sec-table tr.sec-row>td{display:block;padding:0 !important;border:none;width:auto}
#clusters table.users.sec-table tr.sec-row>td:nth-child(3){grid-column:1 / 2;grid-row:2}
#clusters table.users.sec-table tr.sec-row>td.sec-caret{grid-column:2;grid-row:1;align-self:center}
#clusters table.users.sec-table tr.sec-row>td.sec-sev{display:none}
#clusters table.users.sec-table tr.sec-row>td.sec-seen{display:block;grid-column:1 / 2}
#clusters table.users.sec-table tr.sec-detail{display:block}
#clusters table.users.sec-table tr.sec-detail>td{display:block;max-width:none;width:auto}
.sec-grid{grid-template-columns:1fr;gap:4px 0}.sec-grid .sec-k{margin-top:8px}.sec-box{padding:14px}#clusters table.users.sec-table .sec-seen,#clusters table.users.sec-table .sec-sev{display:none}.sec-sev-inline{display:inline-flex;margin-top:4px;font-size:12px}.sec-table .pod-name{max-width:130px}.sec-table .mono-chip,.sec-box .mono-chip{word-break:break-all;white-space:normal}.sec-pre{font-size:11px}}
.mono-chip{font-family:'Geist Mono',ui-monospace,Menlo,monospace;font-size:12px;padding:2px 6px;border:1px solid var(--border);background:var(--input-bg)}
.rs-queued{font-size:12px;font-weight:600;color:var(--green)}
@media(max-width:640px){.rs-table .pod-name{max-width:150px}}
.pill .dot{display:none}
/* in-progress statuses (running jobs, pending pods) pulse, like clusterheart.com */
@keyframes ch-blip{50%{opacity:.5}}
.blip{animation:ch-blip .8s ease-in-out infinite}
@media (prefers-reduced-motion:reduce){.blip{animation:none}}
.dot{width:6px;height:6px;border-radius:50%;background:currentColor}
.empty{padding:48px 18px;text-align:center;color:var(--muted)}

/* --- landing nav --- */
.lp-nav{
  position:sticky;top:0;z-index:10;display:flex;align-items:center;justify-content:space-between;
  padding:14px 40px;background:rgba(7,17,30,.88);backdrop-filter:saturate(180%) blur(14px);
  border-bottom:1px solid var(--border);
}
.lp-nav .brand{margin:0}
.lp-nav-actions{display:flex;align-items:center;gap:22px}
.lp-btn{
  display:inline-flex;align-items:center;justify-content:center;padding:8px 18px;border-radius:9px;
  font-size:13.5px;font-weight:650;cursor:pointer;transition:transform .12s,box-shadow .12s,background .12s;white-space:nowrap;
}
.lp-btn.primary{color:var(--accent-ink);background:var(--accent)}
.lp-btn.primary:hover{transform:translateY(-1px);box-shadow:0 14px 28px -12px rgba(14,165,233,.7);text-decoration:none}
.lp-btn.ghost{color:var(--text);border:1px solid var(--border-strong);background:rgba(255,255,255,.05)}
.lp-btn.ghost:hover{background:rgba(255,255,255,.09);text-decoration:none;transform:translateY(-1px)}
.lp-wrap{max-width:1100px;margin:0 auto;padding:0 40px}

/* --- hero --- */
.lp-hero{padding:clamp(60px,9vw,110px) 0 clamp(48px,7vw,88px)}
.lp-hero-inner{display:grid;grid-template-columns:1fr 1fr;gap:64px;align-items:center}
.lp-hero-text .eyebrow{
  display:inline-flex;align-items:center;gap:7px;margin-bottom:22px;padding:5px 13px;border-radius:999px;
  font-size:11.5px;font-weight:650;letter-spacing:.3px;color:var(--accent);
  background:color-mix(in srgb,var(--accent) 10%,transparent);border:1px solid color-mix(in srgb,var(--accent) 20%,transparent);
}
.lp-hero-text .eyebrow .dot{background:var(--accent);box-shadow:0 0 0 3px color-mix(in srgb,var(--accent) 20%,transparent)}
.lp-hero h1{
  font-size:clamp(34px,5vw,54px);line-height:1.06;font-weight:830;
  letter-spacing:-1.8px;margin:0 0 20px;color:var(--text);
}
.lp-hero h1 .grad{color:var(--accent)}
.lp-hero .lead{font-size:16.5px;line-height:1.7;color:var(--muted);margin:0 0 32px;max-width:460px}
.lp-cta{display:flex;gap:12px;flex-wrap:wrap}
.lp-cta .lp-btn{padding:12px 24px;font-size:15px}
.lp-trust{margin-top:18px;font-size:12.5px;color:var(--faint);display:flex;align-items:center;gap:7px}

/* --- terminal preview --- */
.lp-preview{
  background:#040d18;border:1px solid color-mix(in srgb,var(--accent) 14%,transparent);border-radius:14px;
  overflow:hidden;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;
  box-shadow:0 0 0 1px color-mix(in srgb,var(--accent) 6%,transparent),0 40px 80px -20px rgba(0,0,0,.8),inset 0 1px 0 rgba(255,255,255,.04);
}
.lp-preview-bar{
  display:flex;align-items:center;gap:7px;padding:10px 16px;
  background:rgba(255,255,255,.04);border-bottom:1px solid rgba(255,255,255,.06);
}
.lp-pv-dot{width:11px;height:11px;border-radius:50%}
.lp-pv-title{margin-left:auto;font-size:11px;color:var(--faint)}
.lp-preview-body{padding:16px 18px;display:flex;flex-direction:column;gap:7px}
.lp-prow{display:flex;align-items:center;gap:8px;line-height:1.4}
.lp-plabel{width:58px;font-size:10px;font-weight:700;letter-spacing:.9px;text-transform:uppercase;color:var(--faint);flex-shrink:0}
.lp-pok{color:var(--green);font-weight:600}
.lp-pwarn{color:var(--amber);font-weight:600}
.lp-pdim{color:var(--faint)}
.lp-pdivider{border:none;border-top:1px solid rgba(255,255,255,.06);margin:6px 0}
.lp-ppod{display:flex;align-items:center;gap:9px;padding:3px 0}
.lp-pstatus{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.lp-pstatus.ok{background:var(--green);box-shadow:0 0 5px rgba(52,211,153,.6)}
.lp-pstatus.warn{background:var(--amber);box-shadow:0 0 5px rgba(251,191,36,.5)}
.lp-pname{color:var(--text);flex:1;font-weight:500}
.lp-prep{color:var(--faint);font-size:11.5px}
.lp-pcpu{color:var(--muted);margin-left:auto;font-size:11.5px}

/* --- feature grid --- */
.lp-section{padding:80px 0}
.lp-kicker{text-align:center;font-size:11px;font-weight:700;letter-spacing:1.4px;text-transform:uppercase;color:var(--accent);margin:0 0 10px}
.lp-h2{text-align:center;font-size:clamp(26px,4vw,34px);font-weight:800;letter-spacing:-1px;margin:0 0 10px;color:var(--text)}
.lp-sub2{text-align:center;color:var(--muted);font-size:15.5px;max-width:520px;margin:0 auto 48px}
.lp-grid{
  display:grid;grid-template-columns:repeat(3,1fr);
  border:1px solid var(--border);border-radius:16px;overflow:hidden;
  background:var(--border);gap:1px;
}
.fcard{background:var(--bg);padding:30px 28px;transition:background .16s}
.fcard:hover{background:color-mix(in srgb,var(--accent) 3%,transparent)}
.fcard .ic{
  width:36px;height:36px;border-radius:9px;display:grid;place-items:center;margin-bottom:16px;
  background:color-mix(in srgb,var(--accent) 10%,transparent);border:1px solid color-mix(in srgb,var(--accent) 14%,transparent);color:var(--accent);
}
.fcard h3{font-size:15px;font-weight:720;margin:0 0 7px;letter-spacing:-.2px;color:var(--text)}
.fcard p{font-size:13.5px;line-height:1.6;color:var(--muted);margin:0}

/* --- download section --- */
.dl-install-rec{
  display:inline-flex;align-items:center;padding:2px 9px;border-radius:99px;
  font-size:11px;font-weight:700;letter-spacing:.2px;
  background:color-mix(in srgb,var(--accent) 10%,transparent);color:var(--accent);border:1px solid color-mix(in srgb,var(--accent) 20%,transparent);
}
.dl-cmd-row{
  display:flex;align-items:center;background:rgba(4,13,24,.8);
  border:1px solid var(--border-strong);border-radius:11px;overflow:hidden;
}
.dl-cmd{
  flex:1;padding:13px 16px;font-family:ui-monospace,SFMono-Regular,monospace;
  font-size:13px;color:var(--accent2);user-select:all;white-space:nowrap;overflow:auto;
}
.dl-copy{
  flex-shrink:0;padding:0 15px;height:46px;border:none;border-left:1px solid var(--border);
  background:transparent;color:var(--faint);cursor:pointer;
  display:flex;align-items:center;justify-content:center;transition:background .12s,color .12s;
}
.dl-copy:hover{background:rgba(255,255,255,.06);color:var(--text)}
.dl-mac-card{
  max-width:600px;margin:0 auto 28px;background:var(--panel);
  border:1px solid var(--border-strong);border-radius:16px;padding:26px 26px 20px;
  box-shadow:0 20px 50px -20px rgba(0,0,0,.5);
}
.dl-mac-header{display:flex;align-items:center;gap:10px;margin-bottom:12px}
.dl-mac-icon{
  width:34px;height:34px;border-radius:9px;display:grid;place-items:center;
  background:color-mix(in srgb,var(--accent) 10%,transparent);border:1px solid color-mix(in srgb,var(--accent) 15%,transparent);color:var(--accent);flex-shrink:0;
}
.dl-mac-title{font-size:16px;font-weight:750;letter-spacing:-.3px;color:var(--text)}
.dl-mac-step{font-size:13.5px;color:var(--muted);margin:0 0 12px;line-height:1.55}
.dl-mac-manual{margin:11px 0 0;font-size:12.5px;color:var(--muted)}
.dl-other-grid{display:grid;grid-template-columns:repeat(2,1fr);gap:14px;max-width:420px;margin:0 auto}
.dl-card{
  display:flex;flex-direction:column;align-items:center;gap:5px;text-align:center;
  padding:22px 16px;background:var(--panel);border:1px solid var(--border);border-radius:14px;color:var(--text);
  transition:transform .15s,border-color .15s,box-shadow .15s;
}
.dl-card:hover{transform:translateY(-3px);text-decoration:none;border-color:color-mix(in srgb,var(--accent) 28%,transparent);box-shadow:0 16px 36px -16px color-mix(in srgb,var(--accent) 20%,transparent)}
.dl-ic{
  width:44px;height:44px;border-radius:12px;display:grid;place-items:center;margin-bottom:5px;color:var(--accent);
  background:color-mix(in srgb,var(--accent) 10%,transparent);border:1px solid color-mix(in srgb,var(--accent) 14%,transparent);
}
.dl-os{font-size:15px;font-weight:750;letter-spacing:-.2px;color:var(--text)}
.dl-sub{font-size:12.5px;color:var(--muted)}.dl-sub b{color:var(--text);font-weight:650}
.dl-hint{font-size:11.5px;color:var(--faint);margin-top:2px}
.dl-note{text-align:center;color:var(--muted);font-size:12.5px;max-width:600px;margin:26px auto 0;line-height:1.6}
.dl-note code{background:rgba(255,255,255,.07);border:1px solid var(--border);border-radius:5px;padding:1px 5px;font-size:11.5px}

/* --- footer --- */
.lp-foot{
  max-width:1100px;margin:0 auto;padding:28px 40px;
  display:flex;align-items:center;justify-content:space-between;gap:16px;
  border-top:1px solid var(--border);color:var(--faint);font-size:13px;flex-wrap:wrap;margin-top:72px;
}
.lp-foot .brand{margin:0}.lp-foot .wordmark{font-size:16px}

/* --- responsive --- */
@media(max-width:960px){
  .lp-hero-inner{grid-template-columns:1fr;gap:44px}
  .lp-preview{display:none}
  .lp-hero{padding:64px 0 48px;text-align:center}
  .lp-hero .lead{margin-left:auto;margin-right:auto}
  .lp-cta{justify-content:center}
  .lp-trust{justify-content:center}
}
@media(max-width:820px){
  .lp-nav{padding:13px 20px}
  .lp-wrap{padding:0 20px}
  .lp-grid{grid-template-columns:1fr}
  .lp-section{padding:56px 0}
  .lp-foot{flex-direction:column;text-align:center;gap:10px;padding:28px 20px}
  .dl-mac-card{padding:20px 18px 16px}
  .dl-other-grid{max-width:100%}
}
@media(max-width:560px){
  .lp-nav-actions{gap:12px}
  .lp-nav-actions a:not(.lp-btn){display:none}
}
@media(max-width:480px){
  .center{padding:20px}
  .card{padding:32px 24px}
  h1{font-size:24px}
}
@media(max-width:640px){
  .admin{padding:28px 16px}
  .admin-head{flex-direction:column;align-items:flex-start;gap:6px}
  table.users thead{display:none}
  table.users,table.users tbody,table.users tr,table.users td{display:block;width:100%}
  table.users tbody tr{border-top:none;border-bottom:1px solid var(--border);padding:14px 16px}
  table.users tbody tr:first-child{border-top:1px solid var(--border)}
  table.users td{padding:4px 0;border:none}
  table.users td.cell-act{text-align:left;margin-top:12px}
  table.users td[data-label]::before{content:attr(data-label);display:block;font-size:10.5px;text-transform:uppercase;letter-spacing:.5px;color:var(--muted);margin-bottom:4px}
  .empty{padding:28px 0}
}
</style></head><body>{{template "body" .}}
<script>
function _mark(el,t,k){el.textContent=t;el.className='msg '+(k||'info');}
async function _post(url,payload){
  try{
    const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload),signal:AbortSignal.timeout(12000)});
    let d={};try{d=await r.json();}catch(e){}
    return {ok:r.ok,status:r.status,data:d};
  }catch(e){
    var m=(e&&e.name==='TimeoutError')?'Server took too long. Try again.':'Network error. Is the server running?';
    return {ok:false,status:0,data:{error:m}};
  }
}
// Click the logo: spin it 360° and cycle the colour scheme, the same six as
// clusterheart.com: green, blue, purple, cyan, coral, white.
(function(){
  var S=['green','blue','purple','cyan','coral','white'],A=['blue','purple','cyan','coral'],c=document.documentElement.classList;
  function cur(){if(!c.contains('dark'))return 'white';for(var i=0;i<A.length;i++)if(c.contains(A[i]))return A[i];return 'green';}
  document.querySelectorAll('[data-theme-logo]').forEach(function(el){
    el.addEventListener('click',function(e){
      e.preventDefault();
      var svg=el.matches('svg')?el:el.querySelector('svg');
      if(svg&&svg.animate){svg.animate([{transform:'rotate(0deg)'},{transform:'rotate(360deg)'}],{duration:600,easing:'cubic-bezier(.45,0,.25,1)'});}
      var n=S[(S.indexOf(cur())+1)%S.length];
      c.toggle('dark',n!=='white');A.forEach(function(a){c.toggle(a,a===n);});
      try{localStorage.setItem('ch-theme',n==='white'?'light':'dark');if(n!=='white')localStorage.setItem('ch-accent',n);}catch(_){}
    });
  });
})();
// Wire every password show/hide toggle on the page.
document.querySelectorAll('.pw-eye').forEach(function(btn){
  btn.addEventListener('click',function(){
    var inp=btn.parentElement.querySelector('input');
    var show=inp.type==='password';
    inp.type=show?'text':'password';
    btn.classList.toggle('on',show);
    btn.setAttribute('aria-label',show?'Hide password':'Show password');
  });
});
</script>
{{block "script" .}}{{end}}
</body></html>{{end}}`

// eyeBtn is the reusable show/hide-password control placed inside a .pw-wrap.
const eyeBtn = `<button type="button" class="pw-eye" aria-label="Show password" tabindex="-1"><svg class="eye" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg><svg class="eye-off" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"/><line x1="1" y1="1" x2="23" y2="23"/></svg></button>`

const logoSVG = `<svg class="logo-mark" data-theme-logo role="button" aria-label="Switch colour scheme" width="26" height="26" viewBox="0 0 100 100"><circle cx="50" cy="50" r="15" fill="currentColor"/><circle cx="50" cy="15" r="10.5" fill="currentColor"/><circle cx="80.3" cy="32.5" r="10.5" fill="currentColor"/><circle cx="80.3" cy="67.5" r="10.5" fill="currentColor"/><circle cx="50" cy="85" r="10.5" fill="currentColor"/><circle cx="19.7" cy="67.5" r="10.5" fill="currentColor"/><circle cx="19.7" cy="32.5" r="10.5" fill="currentColor"/></svg>`

const lockSVG = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>`

// bigLogo is the large brand mark shown in the left panel of the auth pages.
const bigLogo = `<span class="logo-mark" data-theme-logo role="button" aria-label="Switch colour scheme" style="display:inline-flex"><svg width="132" height="132" viewBox="0 0 100 100"><circle cx="50" cy="50" r="15" fill="currentColor"/><circle cx="50" cy="15" r="10.5" fill="currentColor"/><circle cx="80.3" cy="32.5" r="10.5" fill="currentColor"/><circle cx="80.3" cy="67.5" r="10.5" fill="currentColor"/><circle cx="50" cy="85" r="10.5" fill="currentColor"/><circle cx="19.7" cy="67.5" r="10.5" fill="currentColor"/><circle cx="19.7" cy="32.5" r="10.5" fill="currentColor"/></svg></span>`

// --- home / landing ------------------------------------------------------

const homeBody = `{{define "body"}}
<nav class="lp-nav">
  <div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
  <div class="lp-nav-actions">
    <a href="/login" style="color:var(--muted);font-weight:500">Sign in</a>
    <a class="lp-btn primary" href="#download">Download</a>
  </div>
</nav>

<header class="lp-hero">
  <div class="lp-wrap">
    <div class="lp-hero-inner">
      <div class="lp-hero-text">
        <span class="eyebrow"><span class="dot"></span>Kubernetes · FinOps · AI</span>
        <h1>Stop switching tabs.<br><span class="grad">See your whole cluster.</span></h1>
        <p class="lead">Live pod health, real cloud costs, and an AI kubectl copilot, in one desktop app that reads your local kubeconfig. Nothing leaves your machine.</p>
        <div class="lp-cta">
          <a class="lp-btn primary" href="#download">Download free</a>
          <a class="lp-btn ghost" href="/signup">Create account</a>
        </div>
        <div class="lp-trust">
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
          Credentials never leave your machine.
        </div>
      </div>
      <div class="lp-preview">
        <div class="lp-preview-bar">
          <span class="lp-pv-dot" style="background:#ff5f57"></span>
          <span class="lp-pv-dot" style="background:#febc2e"></span>
          <span class="lp-pv-dot" style="background:#28c840"></span>
          <span class="lp-pv-title">clusterheart · production</span>
        </div>
        <div class="lp-preview-body">
          <div class="lp-prow">
            <span class="lp-plabel">NODES</span>
            <span class="lp-pok">3 / 3 Ready</span>
            <span class="lp-pdim">&nbsp;· cpu <b style="color:#94a3b8">34%</b> · mem <b style="color:#94a3b8">61%</b></span>
          </div>
          <div class="lp-prow">
            <span class="lp-plabel">PODS</span>
            <span class="lp-pok">47 / 47 Running</span>
          </div>
          <div class="lp-prow">
            <span class="lp-plabel">COST</span>
            <span class="lp-pwarn">$847 today</span>
            <span class="lp-pdim">&nbsp;· ↓12% vs yesterday</span>
          </div>
          <hr class="lp-pdivider">
          <div class="lp-ppod">
            <span class="lp-pstatus ok"></span>
            <span class="lp-pname">api-server</span>
            <span class="lp-prep">2 replicas</span>
            <span class="lp-pcpu">cpu 12%</span>
          </div>
          <div class="lp-ppod">
            <span class="lp-pstatus ok"></span>
            <span class="lp-pname">frontend</span>
            <span class="lp-prep">3 replicas</span>
            <span class="lp-pcpu">cpu 8%</span>
          </div>
          <div class="lp-ppod">
            <span class="lp-pstatus ok"></span>
            <span class="lp-pname">worker</span>
            <span class="lp-prep">5 replicas</span>
            <span class="lp-pcpu">cpu 41%</span>
          </div>
          <div class="lp-ppod">
            <span class="lp-pstatus warn"></span>
            <span class="lp-pname">ml-pipeline</span>
            <span class="lp-prep" style="color:var(--amber)">1 / 2 · Pending</span>
            <span class="lp-pcpu" style="color:var(--amber)">OOMKill</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</header>

<section class="lp-section lp-wrap">
  <p class="lp-kicker">What it does</p>
  <h2 class="lp-h2">Everything in one window</h2>
  <p class="lp-sub2">No context switching. No extra consoles. Just your cluster.</p>
  <div class="lp-grid">
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg></span>
      <h3>Live cluster health</h3>
      <p>Pods, nodes, events and metrics in real time across all your contexts.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="1" x2="12" y2="23"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg></span>
      <h3>Cloud cost insights</h3>
      <p>Real daily spend from Cost Explorer, right next to your live usage.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/></svg></span>
      <h3>Right-sizing</h3>
      <p>Spot phantom resource requests and see exactly how many nodes you can drop.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a5 5 0 0 1 5 5c0 1.5-.5 2-1 3l-1 1v2H9v-2l-1-1c-.5-1-1-1.5-1-3a5 5 0 0 1 5-5Z"/><line x1="9" y1="18" x2="15" y2="18"/><line x1="10" y1="21" x2="14" y2="21"/></svg></span>
      <h3>AI kubectl copilot</h3>
      <p>Ask in plain English. It runs read-only kubectl and explains what it finds.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg></span>
      <h3>Shell, logs &amp; port-forward</h3>
      <p>Exec into containers, stream logs, forward ports, without leaving the app.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg></span>
      <h3>Local &amp; private</h3>
      <p>Reads your local kubeconfig. Cluster credentials never leave your machine.</p>
    </div>
  </div>
</section>

<section id="download" class="lp-section lp-wrap">
  <p class="lp-kicker">Download</p>
  <h2 class="lp-h2">Get the desktop app</h2>
  <p class="lp-sub2">Free · runs locally · reads your kubeconfig.</p>

  <div class="dl-mac-card">
    <div class="dl-mac-header">
      <span class="dl-mac-icon"><svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor"><path d="M16.4 12.9c0-2.1 1.7-3.1 1.8-3.2-1-1.4-2.5-1.6-3-1.6-1.3-.1-2.5.8-3.2.8-.7 0-1.6-.7-2.7-.7-1.4 0-2.7.8-3.4 2.1-1.5 2.5-.4 6.3 1 8.3.7 1 1.5 2.1 2.6 2.1 1 0 1.4-.7 2.7-.7 1.2 0 1.6.7 2.7.7 1.1 0 1.8-1 2.5-2 .8-1.1 1.1-2.2 1.1-2.3-.1 0-2.1-.8-2.1-3.3zM14.3 6.6c.6-.7 1-1.7.9-2.6-.8 0-1.9.6-2.5 1.3-.5.6-1 1.6-.9 2.5.9.1 1.8-.5 2.5-1.2z"/></svg></span>
      <span class="dl-mac-title">macOS</span>
      <span class="dl-install-rec">Apple Silicon &amp; Intel</span>
    </div>
    <p class="dl-mac-step">Open Terminal and run - it downloads, installs, and handles Gatekeeper in one step:</p>
    <div class="dl-cmd-row">
      <code class="dl-cmd" id="install-cmd">curl -sL …/install.sh | bash</code>
      <button class="dl-copy" id="dl-copy-btn" title="Copy">
        <svg id="dl-copy-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
        <svg id="dl-check-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" style="display:none;color:var(--green)"><polyline points="20 6 9 17 4 12"/></svg>
      </button>
    </div>
    <p class="dl-mac-manual">Manual install: <a href="/downloads/Clusterheart-macOS-AppleSilicon.dmg">Apple Silicon .dmg</a> · <a href="/downloads/Clusterheart-macOS-Intel.dmg">Intel .dmg</a> - right-click → Open the first time.</p>
  </div>

  <div class="dl-other-grid">
    <a class="dl-card" href="/downloads/Clusterheart-Windows.exe">
      <span class="dl-ic"><svg width="22" height="22" viewBox="0 0 24 24" fill="currentColor"><path d="M3 5.6l7.2-1v6.9H3V5.6zm0 12.8l7.2 1v-6.8H3v5.8zm8-13.9L21 3v8.5h-10V4.5zm0 15l10 1.5V12.5H11v6z"/></svg></span>
      <span class="dl-os">Windows</span>
      <span class="dl-sub">64-bit · <b>.exe</b></span>
      <span class="dl-hint">More info → Run anyway</span>
    </a>
    <a class="dl-card" href="/downloads/Clusterheart-Linux.AppImage">
      <span class="dl-ic"><svg width="22" height="22" viewBox="0 0 24 24" fill="currentColor"><path d="M12 2c-1.7 0-3 1.6-3 3.4 0 1.1.1 2-.5 3-.7 1-2.3 2.4-2.3 4.9 0 .9.3 1.7.7 2.4-.5.4-1 1-1 1.8 0 1.4 1.6 1.8 3.2 2.2 1 .3 1.4.9 2.9.9s1.9-.6 2.9-.9c1.6-.4 3.2-.8 3.2-2.2 0-.8-.5-1.4-1-1.8.4-.7.7-1.5.7-2.4 0-2.5-1.6-3.9-2.3-4.9-.6-1-.5-1.9-.5-3C15 3.6 13.7 2 12 2zm-1.4 3.1c.4 0 .7.4.7.9s-.3.9-.7.9-.7-.4-.7-.9.3-.9.7-.9zm2.8 0c.4 0 .7.4.7.9s-.3.9-.7.9-.7-.4-.7-.9.3-.9.7-.9z"/></svg></span>
      <span class="dl-os">Linux</span>
      <span class="dl-sub">x86-64 · <b>.AppImage</b></span>
      <span class="dl-hint"><a href="/downloads/">More builds →</a></span>
    </a>
  </div>

  <script>
  (function(){
    var cmd=document.getElementById('install-cmd');
    var btn=document.getElementById('dl-copy-btn');
    var ci=document.getElementById('dl-copy-icon'),ck=document.getElementById('dl-check-icon');
    var text='curl -sL '+window.location.origin+'/install.sh | bash';
    if(cmd) cmd.textContent=text;
    if(btn) btn.addEventListener('click',function(){
      navigator.clipboard.writeText(text).then(function(){
        ci.style.display='none';ck.style.display='';
        setTimeout(function(){ci.style.display='';ck.style.display='none';},2000);
      });
    });
  })();
  </script>
</section>

<footer class="lp-foot">
  <div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
  <span>Kubernetes health, cost &amp; control, on your machine.</span>
  <span><a href="/login">Sign in</a> · <a href="/signup">Sign up</a></span>
</footer>
{{end}}`

// --- login ---------------------------------------------------------------

const loginBody = `{{define "body"}}<div class="auth-split"><aside class="auth-left">` + bigLogo + `</aside><section class="auth-right"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
<p class="kicker">Sign in</p>
<h1>Welcome back.</h1><p class="sub">Access your Clusterheart account.</p>
<label for="email">Email</label>
<input id="email" type="email" autocomplete="email" placeholder="you@company.com">
<label for="pass">Password</label>
<div class="pw-wrap"><input id="pass" type="password" autocomplete="current-password" placeholder="••••••••">` + eyeBtn + `</div>
<button class="btn" id="go">Sign in</button>
<div id="msg" class="msg"></div>
<div class="alt"><a href="/forgot">Forgot password?</a></div>
<div class="alt" style="margin-top:10px">New here? <a href="/signup">Create an account</a></div>
<div class="trust">` + lockSVG + `Credentials never leave your machine.</div>
</div></section></div>{{end}}
{{define "script"}}<script>
var email=document.getElementById('email'),pass=document.getElementById('pass'),go=document.getElementById('go'),msg=document.getElementById('msg');
async function submit(){
  if(!email.value.trim()||!pass.value){_mark(msg,'Enter your email and password.','err');return;}
  go.disabled=true;_mark(msg,'Signing in…','info');
  var res=await _post('/api/login',{email:email.value.trim(),password:pass.value});
  if(!res.ok){_mark(msg,res.data.error||'Sign in failed.','err');go.disabled=false;return;}
  _mark(msg,'Welcome back, redirecting…','ok');location.href='/app';
}
go.addEventListener('click',submit);
pass.addEventListener('keydown',function(e){if(e.key==='Enter')submit();});
email.addEventListener('keydown',function(e){if(e.key==='Enter')pass.focus();});
</script>{{end}}`

// --- signup --------------------------------------------------------------

const signupBody = `{{define "body"}}<div class="auth-split"><aside class="auth-left">` + bigLogo + `</aside><section class="auth-right"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
<p class="kicker">Create account</p>
<h1>Create your account.</h1><p class="sub">Sign up to request access.</p>
<label for="name">Name</label>
<input id="name" type="text" autocomplete="name" placeholder="Jane Doe">
<label for="email">Work email</label>
<input id="email" type="email" autocomplete="email" placeholder="you@company.com">
<label for="pass">Password</label>
<div class="pw-wrap"><input id="pass" type="password" autocomplete="new-password" placeholder="At least 8 characters">` + eyeBtn + `</div>
<button class="btn" id="go">Create account</button>
<div id="msg" class="msg"></div>
<div class="alt">Already have an account? <a href="/login">Sign in</a></div>
<div class="trust">` + lockSVG + `Credentials never leave your machine.</div>
</div></section></div>{{end}}
{{define "script"}}<script>
var nameEl=document.getElementById('name'),email=document.getElementById('email'),pass=document.getElementById('pass'),go=document.getElementById('go'),msg=document.getElementById('msg');
async function submit(){
  go.disabled=true;_mark(msg,'Creating account…','info');
  var res=await _post('/api/signup',{name:nameEl.value.trim(),email:email.value.trim(),password:pass.value});
  if(!res.ok){_mark(msg,res.data.error||'Could not create account.','err');go.disabled=false;return;}
  _mark(msg,'Account created. Redirecting to sign in…','ok');
  setTimeout(function(){location.href='/login';},1200);
}
go.addEventListener('click',submit);
pass.addEventListener('keydown',function(e){if(e.key==='Enter')submit();});
</script>{{end}}`

// --- forgot / reset password ---------------------------------------------

const forgotBody = `{{define "body"}}<div class="auth-split"><aside class="auth-left">` + bigLogo + `</aside><section class="auth-right"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
<p class="kicker">Reset password</p>
<h1>Forgot password?</h1><p class="sub">Enter your email and we'll send you a reset link.</p>
<label for="email">Email</label>
<input id="email" type="email" autocomplete="email" placeholder="you@company.com">
<button class="btn" id="go">Send reset link</button>
<div id="msg" class="msg"></div>
<div class="alt"><a href="/login">Back to sign in</a></div>
</div></section></div>{{end}}
{{define "script"}}<script>
var email=document.getElementById('email'),go=document.getElementById('go'),msg=document.getElementById('msg');
async function submit(){
  if(!email.value.trim()){_mark(msg,'Enter your email.','err');return;}
  go.disabled=true;_mark(msg,'Sending…','info');
  var res=await _post('/api/forgot',{email:email.value.trim()});
  go.disabled=false;
  if(res.ok){_mark(msg,'If an account exists for that email, a reset link is on its way. Check your inbox.','ok');}
  else{_mark(msg,res.data.error||'Something went wrong.','err');}
}
go.addEventListener('click',submit);
email.addEventListener('keydown',function(e){if(e.key==='Enter')submit();});
</script>{{end}}`

const resetBody = `{{define "body"}}<div class="auth-split"><aside class="auth-left">` + bigLogo + `</aside><section class="auth-right"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
<p class="kicker">Reset password</p>
<h1>Set a new password.</h1><p class="sub">Choose a new password for your account.</p>
<label for="pass">New password</label>
<div class="pw-wrap"><input id="pass" type="password" autocomplete="new-password" placeholder="At least 8 characters">` + eyeBtn + `</div>
<label for="pass2">Confirm password</label>
<div class="pw-wrap"><input id="pass2" type="password" autocomplete="new-password" placeholder="Re-enter password">` + eyeBtn + `</div>
<button class="btn" id="go">Reset password</button>
<div id="msg" class="msg"></div>
<div class="alt"><a href="/login">Back to sign in</a></div>
</div></section></div>{{end}}
{{define "script"}}<script>
var pass=document.getElementById('pass'),pass2=document.getElementById('pass2'),go=document.getElementById('go'),msg=document.getElementById('msg');
var token=new URLSearchParams(location.search).get('token')||'';
if(!token){_mark(msg,'This reset link is invalid or incomplete.','err');go.disabled=true;}
async function submit(){
  if(pass.value.length<8){_mark(msg,'Password must be at least 8 characters.','err');return;}
  if(pass.value!==pass2.value){_mark(msg,'Passwords do not match.','err');return;}
  go.disabled=true;_mark(msg,'Resetting…','info');
  var res=await _post('/api/reset',{token:token,password:pass.value});
  if(!res.ok){_mark(msg,res.data.error||'Reset failed.','err');go.disabled=false;return;}
  _mark(msg,'Password updated. Redirecting to sign in…','ok');
  setTimeout(function(){location.href='/login';},1400);
}
go.addEventListener('click',submit);
pass2.addEventListener('keydown',function(e){if(e.key==='Enter')submit();});
</script>{{end}}`

// --- account -------------------------------------------------------------

const accountBody = `{{define "body"}}<div class="auth-split"><aside class="auth-left">` + bigLogo + `</aside><section class="auth-right"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
<p class="kicker">Account</p>
<h1>Hi, {{.Name}}</h1><p class="sub">{{.Email}}</p>
{{if .Approved}}
  <p><span class="pill on"><span class="dot"></span>Approved</span></p>
  <p class="sub" style="margin-top:14px">You're in. Open your dashboard to add clusters and watch their health from any device.</p>
  <a href="/app" class="btn" style="display:block;text-align:center;text-decoration:none">Open dashboard</a>
{{else}}
  <p><span class="pill off"><span class="dot"></span>Pending approval</span></p>
  <p class="sub" style="margin-top:14px">Your account is waiting for an admin to approve it. You'll be able to use the app once approved.</p>
{{end}}
{{if .IsAdmin}}<div class="alt"><a href="/admin">Open admin panel →</a></div>{{end}}
<button class="btn secondary" id="logout">Log out</button>
</div></section></div>{{end}}
{{define "script"}}<script>
document.getElementById('logout').addEventListener('click',async function(){
  await _post('/api/logout',{});location.href='/login';
});
</script>{{end}}`

// --- admin ---------------------------------------------------------------
