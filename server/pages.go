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
/* account page: dashboard shell, profile header, team table */
.app2-main.acct-main{max-width:960px}
.tb-btn{display:inline-flex;align-items:center;height:38px;padding:0 16px;text-decoration:none}
a.act.approve:hover{color:var(--accent-ink)}
.acct-hero{display:flex;align-items:center;gap:18px;flex-wrap:wrap;margin:0 0 26px}
.acct-avatar{width:56px;height:56px;display:flex;align-items:center;justify-content:center;flex:none;font-size:24px;font-weight:800;text-transform:uppercase;color:var(--accent-ink);background:var(--accent)}
.acct-id{flex:1;min-width:0}
.acct-id h1{margin:0;font-size:28px;text-transform:capitalize;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.acct-email{margin:4px 0 0;font-size:13.5px;color:var(--muted);overflow:hidden;text-overflow:ellipsis}
.acct-status{display:flex;align-items:center;gap:16px}
.acct-admin{font-size:13px;color:var(--accent)}
.acct-pending{margin:0;font-size:14px;line-height:1.6;color:var(--muted)}
.team-add{display:flex;gap:8px}
.team-add .toolbar-input{height:40px}
.team-add input{flex:1;min-width:0}
.team-add .act{height:40px;white-space:nowrap}
.role-help{display:flex;flex-wrap:wrap;gap:6px 22px;margin-top:12px;font-size:12.5px;color:var(--muted)}
.role-help b{color:var(--text);font-weight:600}
.acct-table .toolbar-input{height:34px}
.acct-table td{font-size:13.5px;color:var(--muted)}
.ws-now{margin-right:12px}
.acct-table .empty{padding:28px 22px;font-size:13.5px}
.acct-main .card2-body .msg{margin-top:12px;min-height:0}.acct-main .card2-body .msg:empty{display:none}
.acct-main select.toolbar-input{padding-right:30px;background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6' viewBox='0 0 10 6'%3E%3Cpath d='M1 1l4 4 4-4' fill='none' stroke='%23888' stroke-width='1.5'/%3E%3C/svg%3E");background-repeat:no-repeat;background-position:right 11px center;cursor:pointer}
.acct-table td.cell-act .act{padding:7px 14px}
.act.revoke.sure{color:var(--red);border-color:color-mix(in srgb,var(--red) 50%,transparent)}
@media(max-width:640px){.team-add{flex-wrap:wrap}.team-add input{flex-basis:100%}.team-add select{flex:1}.acct-hero{gap:14px}.acct-avatar{width:46px;height:46px;font-size:20px}.acct-id h1{font-size:23px}.acct-status{width:100%}.tb-btn{height:34px;padding:0 12px}}
.alt a{color:var(--accent);font-weight:600}
.trust{margin-top:24px;padding-top:22px;border-top:1px solid var(--border);display:flex;align-items:center;justify-content:center;gap:8px;font-size:12.5px;color:var(--faint)}
.msg{min-height:20px;margin-top:16px;font-size:13px}
.msg.err{color:var(--red)}.msg.ok{color:var(--green)}.msg.info{color:var(--muted)}

/* --- admin (sidebar shell, Apple-style) --- */
.admin-shell{display:flex;min-height:100vh}
.admin-nav{width:238px;flex-shrink:0;background:var(--surface2);border-right:1px solid var(--border);padding:22px 14px;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow:hidden;white-space:nowrap;transition:width .28s cubic-bezier(.4,0,.2,1),padding .28s cubic-bezier(.4,0,.2,1),opacity .2s ease}
.an-brand{display:flex;flex-direction:row;align-items:center;justify-content:flex-end;gap:10px;padding:2px 14px 14px;margin:0 -14px 12px;border-bottom:1px solid var(--border)}
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
.notice-bar{margin:0 0 20px;padding:12px 16px;font-size:13.5px;color:var(--text);background:color-mix(in srgb,var(--accent) 10%,var(--surface2));border:1px solid color-mix(in srgb,var(--accent) 40%,transparent)}
.tb-ws{margin-right:auto;max-width:42vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12.5px;font-weight:600;color:var(--muted);padding:5px 10px;border:1px solid var(--border-strong)}
.tb-ws.shared{color:var(--accent);border-color:color-mix(in srgb,var(--accent) 45%,transparent)}
.menu-label{padding:6px 12px 2px;font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--faint)}
.ws-pick.on{color:var(--accent)}.ws-pick.on::after{content:' ✓'}
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
.app2-main{flex:1 1 auto;width:100%;max-width:1440px;margin:0 auto;padding:30px 28px 64px}
@media(max-width:640px){.topbar{padding:12px 16px}.app2-main{padding:22px 16px 48px}}
/* cluster list: compact responsive card grid (no horizontal scroll) */
.cl-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(270px,1fr));gap:14px}
.cl-card{display:flex;flex-direction:column;gap:14px;padding:16px 18px;background:var(--surface2);border:1px solid var(--border);transition:border-color .14s,background .14s}
.cl-card:hover{border-color:var(--border-strong);background:var(--hover)}
.cl-grid>.cl-card,.tl-grid>.tl-card .tl-head{cursor:grab;user-select:none;-webkit-user-select:none;-webkit-touch-callout:none;position:relative;scale:1;transition:border-color .14s,background .14s,scale .2s cubic-bezier(.2,.8,.2,1),box-shadow .2s}
.cl-grid.arranging,.cl-grid.arranging *,.tl-grid.arranging,.tl-grid.arranging *{cursor:grabbing}
.cl-grid.arranging>.cl-card,.tl-grid.arranging>.tl-card{will-change:transform}
.cl-card.lifted,.tl-card.lifted{z-index:20;pointer-events:none;scale:1.035;border-color:var(--accent);background:var(--panel);box-shadow:0 26px 56px -20px rgba(0,0,0,.65),0 0 0 1px color-mix(in srgb,var(--accent) 35%,transparent)}
.cl-card.settling,.tl-card.settling{z-index:20}
.tl-grid>.tl-card{position:relative;scale:1;transition:scale .2s cubic-bezier(.2,.8,.2,1),box-shadow .2s,border-color .14s}
.tl-grid>.tl-card .tl-head{scale:none;transition:none;position:static}
@media (prefers-reduced-motion:reduce){.cl-grid>.cl-card,.tl-grid>.tl-card,.cl-card.settling,.tl-card.settling{transition:none!important}}
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
/* Server overview: system strip + processes beside GPUs and disks */
.ov-sys{margin-bottom:16px}
.ov-specs{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr))}
.ov-spec{padding:14px 18px;border-right:1px solid var(--border);min-width:0}
.ov-spec:last-child{border-right:none}
.ov-k{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--faint);margin-bottom:6px}
.ov-v{font-size:14px;color:var(--text);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.ov-grid{display:grid;grid-template-columns:minmax(0,1.35fr) minmax(0,1fr);gap:16px;align-items:start}
.ov-grid.one{grid-template-columns:minmax(0,1fr)}
.ov-grid .card2,.ov-side .card2{margin:0}
.ov-side{display:flex;flex-direction:column;gap:16px;min-width:0}
.ov-procs .health-scroll{max-height:560px}
@media (max-width:1000px){.ov-grid{grid-template-columns:minmax(0,1fr)}}
@media (max-width:640px){.ov-specs{grid-template-columns:repeat(2,minmax(0,1fr))}.ov-spec{border-right:none;border-bottom:1px solid var(--border)}}
/* Server timeline */
.tl-bar{display:flex;align-items:center;justify-content:space-between;gap:10px;margin:0 0 14px;flex-wrap:wrap;position:relative}
.tl-picker{position:absolute;top:calc(100% + 6px);left:0;z-index:40;width:min(440px,calc(100vw - 32px));padding:14px;background:var(--panel);border:1px solid var(--border-strong);box-shadow:0 18px 44px -16px rgba(0,0,0,.55)}
.tl-pick-row{display:grid;grid-template-columns:1fr 1fr;gap:10px}
.tl-pick-row label{display:flex;flex-direction:column;gap:6px;font-size:12px;font-weight:600;color:var(--muted)}
.tl-pick-row input{width:100%;min-width:0;color-scheme:inherit}
.tl-pick-err{margin:10px 0 0;font-size:12.5px;color:var(--red)}
.tl-pick-actions{display:flex;align-items:center;gap:8px;margin-top:12px}
.tl-pick-note{margin-right:auto;font-size:12px;color:var(--faint)}
@media (max-width:480px){.tl-pick-row{grid-template-columns:1fr}}
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
.u-cnt{font-variant-numeric:tabular-nums;white-space:nowrap}.u-cnt b{font-size:15px;font-weight:650;color:var(--text)}
.u-in{font-size:11.5px;color:var(--muted);white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:220px}
.u-off{font-size:12px;color:var(--amber,var(--muted));margin-left:4px}.u-none{color:var(--faint)}
.adm-totals{display:flex;gap:0;margin-top:14px;border:1px solid var(--border);background:var(--surface2);width:max-content;max-width:100%}
.adm-totals>div{display:flex;align-items:baseline;gap:8px;white-space:nowrap;padding:12px 22px;border-right:1px solid var(--border)}.adm-totals>div:last-child{border-right:none}
.adm-totals b{font-size:20px;font-weight:700;color:var(--text);font-variant-numeric:tabular-nums}.adm-totals span{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--faint)}
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

@media(max-width:480px){
  .center{padding:20px}
  .card{padding:32px 24px}
  h1{font-size:24px}
}
@media(max-width:640px){
  .admin{padding:28px 16px}
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
// A team invite links here with the invited email filled in.
var _inv=new URLSearchParams(location.search).get('email');if(_inv)email.value=_inv;
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

const accountBody = `{{define "body"}}<div class="app2">
<header class="topbar">
  <a class="tb-brand" href="/app">` + logoSVG + `<span class="wordmark">Clusterheart</span></a>
  <div class="tb-right">
    {{if .Approved}}<a class="act approve tb-btn" href="/app">Dashboard</a>{{end}}
    <button class="act revoke tb-btn" id="logout">Log out</button>
  </div>
</header>
<main class="app2-main acct-main">
  <p class="kicker">Account</p>
  <section class="acct-hero">
    <div class="acct-avatar" aria-hidden="true">{{.Initial}}</div>
    <div class="acct-id"><h1>{{.Name}}</h1><p class="acct-email">{{.Email}}</p></div>
    <div class="acct-status">{{if .Approved}}<span class="pill on">Approved</span>{{else}}<span class="pill off">Pending approval</span>{{end}}{{if .IsAdmin}}<a class="acct-admin" href="/admin">Admin panel &rarr;</a>{{end}}</div>
  </section>
{{if .Approved}}
  <section class="card2" id="spaces" hidden>
    <div class="card2-head"><p class="card2-title">Workspaces</p><p class="card2-desc">Yours, plus the teams you're on. The one you open is used everywhere in the dashboard.</p></div>
    <table class="users acct-table"><tbody id="space-list"></tbody></table>
  </section>
  <section class="card2">
    <div class="card2-head"><p class="card2-title">Team</p><p class="card2-desc">People you add see your clusters and servers. They get an email; new people sign up with that address and skip the approval wait.</p></div>
    <div class="card2-body">
      <div class="team-add"><input id="t-email" class="toolbar-input" type="email" placeholder="teammate@company.com" autocomplete="off" aria-label="Email"><select id="t-role" class="toolbar-input" aria-label="Role"><option value="member">Member</option><option value="admin">Admin</option></select><button id="t-add" class="act approve">Add to team</button></div>
      <div class="role-help"><span><b>Member</b> sees everything and uses AI</span><span><b>Admin</b> also adds, renames and removes clusters and servers, and acts on fixes</span></div>
      <div id="t-msg" class="msg" role="status"></div>
    </div>
    <table class="users acct-table"><thead><tr><th>Person</th><th>Status</th><th>Role</th><th></th></tr></thead><tbody id="team-list"><tr><td colspan="4" class="empty">Loading…</td></tr></tbody></table>
  </section>
{{else}}
  <section class="card2"><div class="card2-body"><p class="acct-pending">Your account is waiting for an admin to approve it. You'll get in as soon as it's approved. If a teammate added you, sign in with the email they used.</p></div></section>
{{end}}
</main>
</div>{{end}}
{{define "script"}}<script>
document.getElementById('logout').addEventListener('click',async function(){
  await _post('/api/logout',{});location.href='/login';
});
(function(){
  var list=document.getElementById('team-list');if(!list)return;
  function esc(s){return String(s||'').replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
  var msg=document.getElementById('t-msg'),em=document.getElementById('t-email'),role=document.getElementById('t-role'),add=document.getElementById('t-add');
  async function api(method,url,body){
    var r=await fetch(url,{method:method,headers:{'Content-Type':'application/json','Accept':'application/json'},body:body?JSON.stringify(body):undefined});
    var d=await r.json().catch(function(){return {};});return {ok:r.ok,data:d};
  }
  function roleSel(m){return '<select class="toolbar-input t-role" data-email="'+esc(m.email)+'" aria-label="Role for '+esc(m.email)+'"><option value="member"'+(m.role==='member'?' selected':'')+'>Member</option><option value="admin"'+(m.role==='admin'?' selected':'')+'>Admin</option></select>';}
  async function load(){
    var r=await api('GET','/api/team');
    if(!r.ok){list.innerHTML='<tr><td colspan="4" class="empty">Could not load your team.</td></tr>';return;}
    var t=r.data.team||[],ws=r.data.workspaces||[];
    list.innerHTML=t.length?t.map(function(m){
      return '<tr><td class="cell-user"><div class="u-name">'+esc(m.name||m.email.split('@')[0])+'</div><div class="u-email">'+esc(m.email)+'</div></td>'+
        '<td data-label="Status">'+(m.joined?'<span class="pill on">Joined</span>':'<span class="pill off">Invited</span>')+'</td>'+
        '<td data-label="Role">'+roleSel(m)+'</td>'+
        '<td class="cell-act"><button class="act revoke team-x" data-email="'+esc(m.email)+'">Remove</button></td></tr>';
    }).join(''):'<tr><td colspan="4" class="empty">No one yet. Add a teammate above.</td></tr>';
    var box=document.getElementById('spaces');
    if(ws.length>1){
      box.hidden=false;
      document.getElementById('space-list').innerHTML=ws.map(function(w){
        return '<tr><td class="cell-user"><div class="u-name">'+esc(w.name)+'</div><div class="u-email">'+(w.owner?esc(w.owner):'Your own clusters and servers')+'</div></td>'+
          '<td data-label="Role">'+(w.owner?(w.role==='admin'?'Admin':'Member'):'Owner')+'</td>'+
          '<td class="cell-act">'+(w.active?'<span class="pill on ws-now">Open now</span>':'<button class="act approve ws-open" data-owner="'+esc(w.owner)+'">Open</button>')+
          (w.owner?' <button class="act revoke team-x ws-leave" data-owner="'+esc(w.owner)+'">Leave</button>':'')+'</td></tr>';
      }).join('');
    }else box.hidden=true;
  }
  add.addEventListener('click',async function(){
    var e=em.value.trim();if(!e){em.focus();return;}
    add.disabled=true;_mark(msg,'Adding…','info');
    var r=await api('POST','/api/team',{email:e,role:role.value});
    add.disabled=false;
    if(!r.ok){_mark(msg,r.data.error||'Could not add.','err');return;}
    _mark(msg,(r.data.joined?'Added. They can open your workspace now. We emailed ':'Invited. We emailed ')+e+'.','ok');em.value='';role.value='member';load();
  });
  em.addEventListener('keydown',function(e){if(e.key==='Enter')add.click();});
  document.addEventListener('change',async function(e){
    var s=e.target.closest&&e.target.closest('.t-role');if(!s)return;
    s.disabled=true;var r=await api('PATCH','/api/team',{email:s.dataset.email,role:s.value});s.disabled=false;
    _mark(msg,r.ok?'Role updated.':(r.data.error||'Could not update.'),r.ok?'ok':'err');
  });
  // Two-step remove/leave: the first click asks, the second does it.
  document.addEventListener('click',async function(e){
    var b=e.target.closest&&e.target.closest('.team-x,.ws-open');if(!b)return;
    if(b.classList.contains('ws-open')){b.disabled=true;var o=await api('POST','/api/workspace',{owner:b.dataset.owner});if(o.ok)location.href='/app';else b.disabled=false;return;}
    var label=b.classList.contains('ws-leave')?'Leave':'Remove';
    if(!b.classList.contains('sure')){b.classList.add('sure');b.textContent='Sure?';setTimeout(function(){if(b.isConnected){b.classList.remove('sure');b.textContent=label;}},3000);return;}
    b.disabled=true;
    var r=b.classList.contains('ws-leave')?await api('POST','/api/workspace',{owner:b.dataset.owner,leave:true}):await api('DELETE','/api/team?email='+encodeURIComponent(b.dataset.email));
    _mark(msg,r.ok?(label==='Leave'?'You left that workspace.':'Removed. Their access ended.'):(r.data.error||'Could not do that.'),r.ok?'ok':'err');
    load();
  });
  load();
})();
</script>{{end}}`

// --- admin ---------------------------------------------------------------
