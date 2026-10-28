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
<script>(function(){var t=localStorage.getItem('ch-theme')||'dark';if(t==='dark')document.documentElement.classList.add('dark');})();</script>
<style>
@import url('https://fonts.googleapis.com/css2?family=Geist:wght@300..800&family=Geist+Mono:wght@400..700&display=swap');
:root{
  --bg:#ffffff;--panel:#ffffff;--surface2:#fafafa;--input-bg:#ffffff;
  --text:#0a0a0a;--muted:#525252;--faint:#a3a3a3;
  --border:#e5e5e5;--border-strong:#d4d4d4;
  --accent:#0a0a0a;--accent2:#262626;--accent-ink:#ffffff;
  --active-bg:#ffffff;--hover:rgba(0,0,0,.05);--focus:rgba(0,0,0,.09);
  --green:#16a34a;--amber:#d97706;--red:#dc2626;
}
.dark{
  --bg:#000000;--panel:#000000;--surface2:#06090f;--input-bg:#0a0e16;
  --text:#E6F1FD;--muted:#8294AE;--faint:#5A6880;
  --border:#1C2A40;--border-strong:#2A3C58;
  --accent:#8EC6FA;--accent2:#BBDDFC;--accent-ink:#04070F;
  --active-bg:#0e1726;--hover:rgba(142,198,250,.08);--focus:rgba(142,198,250,.18);
  --green:#4ade80;--amber:#fb923c;--red:#f87171;
}
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
.admin-nav{width:238px;flex-shrink:0;background:var(--surface2);border-right:1px solid var(--border);padding:22px 14px;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow:hidden;white-space:nowrap;transition:width .25s ease,padding .25s ease,opacity .2s ease}
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
.admin-main{flex:1 1 auto;min-width:0;padding:52px 56px;max-width:1040px}
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
.card2{background:var(--surface2);border:1px solid var(--border);border-radius:0;box-shadow:0 24px 60px -30px rgba(0,0,0,.8);overflow:hidden;margin:0 0 20px}
.card2-head{padding:20px 22px 16px;border-bottom:1px solid var(--border)}
.card2-title{font-size:15px;font-weight:650;letter-spacing:-.2px;color:var(--text);margin:0}
.card2-desc{font-size:13px;color:var(--muted);margin:3px 0 0}
.card2-body{padding:18px 22px 20px}
/* scrollable table area with a pinned header (for long pod/job lists) */
.tscroll{max-height:440px;overflow:auto;border-top:1px solid var(--border)}
.tscroll table.users th{position:sticky;top:0;background:var(--input-bg);z-index:1}
/* collapsible sidebar via the burger toggle */
.admin-shell.nav-hidden .admin-nav{width:0;padding-left:0;padding-right:0;border-right:none;opacity:0}
.nav-burger{display:inline-flex;align-items:center;justify-content:center;background:none;border:none;color:var(--muted);cursor:pointer;font-size:21px;line-height:1;padding:2px;flex-shrink:0}
.nav-burger:hover{color:var(--accent)}
#nav-reopen{display:none;margin-bottom:14px}
.nav-hidden #nav-reopen{display:inline-flex}
.seg{display:inline-flex;border:1px solid var(--border-strong);overflow:hidden;flex-shrink:0}
.seg-btn{padding:6px 13px;font-size:12.5px;font-weight:600;background:transparent;color:var(--muted);border:none;cursor:pointer;font-family:inherit}
.seg-btn.active{background:var(--accent);color:var(--accent-ink)}
.seg-btn+.seg-btn{border-left:1px solid var(--border-strong)}
.toolbar-input{width:auto;padding:7px 10px;font-size:12.5px;background:var(--input-bg);border:1px solid var(--border-strong);color:var(--text);font-family:inherit;outline:none;appearance:none;-webkit-appearance:none}
.toolbar-input:focus{border-color:var(--accent)}
/* four-up stat grid (desktop) -> two-up on phones */
.statgrid{display:grid;grid-template-columns:repeat(4,1fr);gap:10px;margin:16px 0}
.statcard{padding:13px 14px;background:var(--surface2);border:1px solid var(--border);min-width:0}
.stat-label{font-size:11px;color:var(--faint);line-height:1.25}
.stat-val{font-size:20px;font-weight:700;color:var(--text);margin-top:2px;white-space:nowrap}
.stat-sub{font-size:10.5px;color:var(--faint);margin-top:3px}
.admin-main h1{font-size:22px;letter-spacing:-.5px}
/* apple-style small modal */
.modal-bg{position:fixed;inset:0;background:rgba(0,0,0,.55);backdrop-filter:blur(3px);display:flex;align-items:center;justify-content:center;z-index:200;padding:20px;animation:mfade .15s ease}
@keyframes mfade{from{opacity:0}to{opacity:1}}
.modal{width:100%;max-width:300px;background:var(--surface2);border:1px solid var(--border-strong);border-radius:14px;padding:20px;text-align:center;box-shadow:0 24px 64px -18px rgba(0,0,0,.85)}
.modal h3{margin:0 0 6px;font-size:16px;font-weight:650;color:var(--text)}
.modal p{margin:0 0 14px;font-size:13px;color:var(--muted);line-height:1.5}
.modal input{width:100%;margin:2px 0 14px;font-size:14px;padding:9px 11px}
.modal-actions{display:flex;gap:8px}
.modal-actions button{flex:1;padding:9px;font-size:13.5px;font-weight:600;font-family:inherit;cursor:pointer;border:1px solid var(--border-strong);background:transparent;color:var(--text);border-radius:8px}
.modal-actions .primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink)}
.modal-actions .danger{background:#b91c1c;border-color:#b91c1c;color:#fff}
@media(max-width:720px){
  .admin-main{padding:18px 14px}
  .admin-main h1{font-size:19px}
  .card2{overflow-x:auto}
  .card2-body,.card2-head{padding-left:14px;padding-right:14px}
  table.users th,table.users td{padding:10px 12px;font-size:12.5px}
  .tscroll{max-height:62vh}
  .nav-burger{display:none}
  .an-me{display:none}
  .admin-nav{overflow:visible;white-space:normal}
  .an-brand{border-bottom:none;margin:0 14px 0 0;padding:0;justify-content:flex-start}
  .statgrid{gap:6px}
  .statcard{padding:9px 8px}
  .stat-label{font-size:9.5px}
  .stat-val{font-size:16px}
  .stat-sub{display:none}
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
.u-email{font-family:'Geist Mono',ui-monospace,SFMono-Regular,monospace;font-size:12px;color:var(--muted);margin-top:2px}
.u-time{font-size:13px;color:var(--text);white-space:nowrap}
.u-ver{font-family:'Geist Mono',ui-monospace,monospace;font-size:12.5px;color:var(--muted)}
.cell-act{text-align:right;white-space:nowrap}
.pill{display:inline-flex;align-items:center;gap:7px;padding:4px 11px;border-radius:999px;font-size:12px;font-weight:600}
.pill.on{background:rgba(74,222,128,.12);color:var(--green)}
.pill.off{background:rgba(251,146,60,.12);color:var(--amber)}
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
.lp-btn.primary{color:#fff;background:linear-gradient(135deg,#0ea5e9,#0284c7);box-shadow:0 8px 22px -10px rgba(14,165,233,.55)}
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
  background:rgba(56,189,248,.1);border:1px solid rgba(56,189,248,.2);
}
.lp-hero-text .eyebrow .dot{background:var(--accent);box-shadow:0 0 0 3px rgba(56,189,248,.2)}
.lp-hero h1{
  font-size:clamp(34px,5vw,54px);line-height:1.06;font-weight:830;
  letter-spacing:-1.8px;margin:0 0 20px;color:var(--text);
}
.lp-hero h1 .grad{
  background:linear-gradient(110deg,var(--accent),var(--accent2));
  -webkit-background-clip:text;background-clip:text;color:transparent;
}
.lp-hero .lead{font-size:16.5px;line-height:1.7;color:var(--muted);margin:0 0 32px;max-width:460px}
.lp-cta{display:flex;gap:12px;flex-wrap:wrap}
.lp-cta .lp-btn{padding:12px 24px;font-size:15px}
.lp-trust{margin-top:18px;font-size:12.5px;color:var(--faint);display:flex;align-items:center;gap:7px}

/* --- terminal preview --- */
.lp-preview{
  background:#040d18;border:1px solid rgba(56,189,248,.14);border-radius:14px;
  overflow:hidden;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;
  box-shadow:0 0 0 1px rgba(56,189,248,.06),0 40px 80px -20px rgba(0,0,0,.8),inset 0 1px 0 rgba(255,255,255,.04);
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
.fcard:hover{background:rgba(56,189,248,.03)}
.fcard .ic{
  width:36px;height:36px;border-radius:9px;display:grid;place-items:center;margin-bottom:16px;
  background:rgba(56,189,248,.1);border:1px solid rgba(56,189,248,.14);color:var(--accent);
}
.fcard h3{font-size:15px;font-weight:720;margin:0 0 7px;letter-spacing:-.2px;color:var(--text)}
.fcard p{font-size:13.5px;line-height:1.6;color:var(--muted);margin:0}

/* --- download section --- */
.dl-install-rec{
  display:inline-flex;align-items:center;padding:2px 9px;border-radius:99px;
  font-size:11px;font-weight:700;letter-spacing:.2px;
  background:rgba(56,189,248,.1);color:var(--accent);border:1px solid rgba(56,189,248,.2);
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
  background:rgba(56,189,248,.1);border:1px solid rgba(56,189,248,.15);color:var(--accent);flex-shrink:0;
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
.dl-card:hover{transform:translateY(-3px);text-decoration:none;border-color:rgba(56,189,248,.28);box-shadow:0 16px 36px -16px rgba(56,189,248,.2)}
.dl-ic{
  width:44px;height:44px;border-radius:12px;display:grid;place-items:center;margin-bottom:5px;color:var(--accent);
  background:rgba(56,189,248,.1);border:1px solid rgba(56,189,248,.14);
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
// Click the logo: spin it 360° and flip the colour scheme (light <-> dark).
document.querySelectorAll('[data-theme-logo]').forEach(function(el){
  el.addEventListener('click',function(e){
    e.preventDefault();
    var svg=el.matches('svg')?el:el.querySelector('svg');
    if(svg&&svg.animate){svg.animate([{transform:'rotate(0deg)'},{transform:'rotate(360deg)'}],{duration:600,easing:'cubic-bezier(.45,0,.25,1)'});}
    var d=document.documentElement.classList.toggle('dark');
    try{localStorage.setItem('ch-theme',d?'dark':'light');}catch(_){}
  });
});
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

const logoSVG = `<svg class="logo-mark" data-theme-logo role="button" aria-label="Switch colour scheme" width="26" height="26" viewBox="0 0 100 100"><circle cx="50" cy="50" r="15" fill="#8EC6FA"/><circle cx="50" cy="15" r="10.5" fill="#8EC6FA"/><circle cx="80.3" cy="32.5" r="10.5" fill="#8EC6FA"/><circle cx="80.3" cy="67.5" r="10.5" fill="#8EC6FA"/><circle cx="50" cy="85" r="10.5" fill="#8EC6FA"/><circle cx="19.7" cy="67.5" r="10.5" fill="#8EC6FA"/><circle cx="19.7" cy="32.5" r="10.5" fill="#8EC6FA"/></svg>`

const lockSVG = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>`

// bigLogo is the large brand mark shown in the left panel of the auth pages.
const bigLogo = `<span class="logo-mark" data-theme-logo role="button" aria-label="Switch colour scheme" style="display:inline-flex"><svg width="132" height="132" viewBox="0 0 100 100"><circle cx="50" cy="50" r="15" fill="#8EC6FA"/><circle cx="50" cy="15" r="10.5" fill="#8EC6FA"/><circle cx="80.3" cy="32.5" r="10.5" fill="#8EC6FA"/><circle cx="80.3" cy="67.5" r="10.5" fill="#8EC6FA"/><circle cx="50" cy="85" r="10.5" fill="#8EC6FA"/><circle cx="19.7" cy="67.5" r="10.5" fill="#8EC6FA"/><circle cx="19.7" cy="32.5" r="10.5" fill="#8EC6FA"/></svg></span>`

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
