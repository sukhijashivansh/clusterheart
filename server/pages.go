package main

import (
	"html/template"
	"net/http"
)

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never let the browser cache these pages — a stale copy means old JS runs
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
	adminTmpl   = page(adminBody)
)

// --- shared layout + design system --------------------------------------

const layout = `{{define "layout"}}<!DOCTYPE html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Clusterheart</title>
<style>
:root{
  --bg:#ffffff;--panel:#f4f7fd;--text:#0f172a;--muted:#64748b;--faint:#94a3b8;
  --border:#e5e9f2;--border-strong:#d6ddec;
  --accent:#2F6BEF;--accent2:#5b9dff;--accent-ink:#1e4fd0;
  --green:#0f9d63;--amber:#c9880f;--red:#e5484d;
}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%}
body{
  background:var(--bg);color:var(--text);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,sans-serif;
  -webkit-font-smoothing:antialiased;line-height:1.5;
  background-image:
    radial-gradient(680px 440px at 12% -8%,rgba(47,107,239,.10),transparent 60%),
    radial-gradient(720px 520px at 108% 112%,rgba(91,157,255,.12),transparent 55%);
  background-attachment:fixed;
}
a{color:var(--accent);text-decoration:none;font-weight:600}a:hover{text-decoration:underline}
.center{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:32px}
.card{
  width:100%;max-width:400px;padding:40px 36px;
  background:#fff;
  border:1px solid var(--border);border-radius:18px;
  box-shadow:0 24px 60px -28px rgba(15,23,42,.22),0 2px 6px -2px rgba(15,23,42,.06);
}
.brand{display:flex;align-items:center;gap:11px;margin-bottom:22px}
.brand svg{filter:drop-shadow(0 4px 10px rgba(47,107,239,.28))}
.wordmark{font-size:20px;font-weight:700;letter-spacing:-.4px;color:var(--text)}
.wordmark .a{color:var(--accent)}
h1{font-size:23px;font-weight:750;letter-spacing:-.5px;margin:0 0 7px}
.sub{color:var(--muted);font-size:13.5px;margin:0 0 26px}
label{display:block;font-size:12.5px;font-weight:600;color:var(--text);margin:18px 0 8px}
input{
  width:100%;padding:12px 14px;font-size:14px;color:var(--text);
  background:#fff;border:1px solid var(--border-strong);border-radius:11px;outline:none;
  transition:border-color .15s,box-shadow .15s;
}
input:focus{border-color:var(--accent);box-shadow:0 0 0 4px rgba(47,107,239,.14)}
input::placeholder{color:#aab4c5}
.btn{
  width:100%;margin-top:26px;padding:13px 16px;font-size:14px;font-weight:650;color:#fff;cursor:pointer;
  background:linear-gradient(135deg,#3B74F0,#2F6BEF);border:none;border-radius:11px;
  box-shadow:0 12px 24px -10px rgba(47,107,239,.7);transition:transform .12s,box-shadow .12s,opacity .12s;
}
.btn:hover{transform:translateY(-1px);box-shadow:0 16px 30px -12px rgba(47,107,239,.75)}
.btn:active{transform:translateY(0)}
.btn:disabled{opacity:.55;cursor:default;transform:none;box-shadow:none}
.btn.secondary{margin-top:12px;background:#fff;color:var(--text);border:1px solid var(--border-strong);box-shadow:none}
.btn.secondary:hover{background:#f7f9fc;box-shadow:none}
.alt{margin-top:22px;font-size:13px;color:var(--muted);text-align:center}
.msg{min-height:20px;margin-top:16px;font-size:13px}
.msg.err{color:var(--red)}.msg.ok{color:var(--green)}.msg.info{color:var(--muted)}
.pill{display:inline-flex;align-items:center;gap:6px;padding:5px 12px;border-radius:20px;font-size:12px;font-weight:600}
.pill.on{background:rgba(15,157,99,.10);color:var(--green);border:1px solid rgba(15,157,99,.2)}
.pill.off{background:rgba(201,136,15,.10);color:var(--amber);border:1px solid rgba(201,136,15,.2)}
.dot{width:6px;height:6px;border-radius:50%;background:currentColor}

/* --- admin panel --- */
.admin{max-width:880px;margin:0 auto;padding:52px 24px}
.admin-head{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:6px}
.admin-user{font-size:13px;color:var(--muted);white-space:nowrap}
.count{display:inline-flex;align-items:center;justify-content:center;min-width:20px;height:20px;padding:0 7px;margin-right:8px;border-radius:20px;background:rgba(201,136,15,.14);color:var(--amber);font-size:12px;font-weight:700;vertical-align:middle}
.panel{background:#fff;border:1px solid var(--border);border-radius:16px;overflow:hidden;box-shadow:0 24px 60px -34px rgba(15,23,42,.22),0 2px 6px -2px rgba(15,23,42,.05)}
table.users{width:100%;border-collapse:collapse}
table.users th{text-align:left;padding:14px 18px;font-size:11px;text-transform:uppercase;letter-spacing:.6px;color:var(--muted);font-weight:700;background:#f8fafc;border-bottom:1px solid var(--border)}
table.users td{padding:15px 18px;vertical-align:middle}
table.users tbody tr{border-top:1px solid var(--border);transition:background .12s}
table.users tbody tr:hover{background:#f8fafc}
.u-name{font-weight:650;font-size:14px}
.u-email{font-family:ui-monospace,SFMono-Regular,monospace;font-size:12px;color:var(--muted);margin-top:2px}
.u-time{font-size:13px;color:var(--muted)}
.cell-act{text-align:right;white-space:nowrap}
.act{padding:7px 16px;border-radius:8px;font-size:12.5px;font-weight:650;cursor:pointer;transition:transform .12s,opacity .12s,box-shadow .12s,border-color .12s,color .12s,background .12s}
.act:hover{transform:translateY(-1px)}
.act:active{transform:translateY(0)}
.act:disabled{opacity:.55;cursor:default;transform:none;box-shadow:none}
.act.approve{border:none;color:#fff;background:linear-gradient(135deg,#0f9d63,#14b877);box-shadow:0 8px 20px -8px rgba(15,157,99,.55)}
.act.revoke{border:1px solid var(--border-strong);background:#fff;color:var(--muted)}
.act.revoke:hover{color:var(--red);border-color:rgba(229,72,77,.4);background:#fff5f5}
.empty{padding:40px 18px;text-align:center;color:var(--muted)}

/* --- landing page --- */
.lp-nav{position:sticky;top:0;z-index:10;display:flex;align-items:center;justify-content:space-between;
  padding:16px 32px;background:rgba(255,255,255,.8);backdrop-filter:saturate(160%) blur(10px);border-bottom:1px solid var(--border)}
.lp-nav .brand{margin:0}
.lp-nav-actions{display:flex;align-items:center;gap:18px}
.lp-btn{display:inline-flex;align-items:center;justify-content:center;padding:9px 18px;border-radius:10px;
  font-size:13.5px;font-weight:650;cursor:pointer;transition:transform .12s,box-shadow .12s,background .12s;white-space:nowrap}
.lp-btn.primary{color:#fff;background:linear-gradient(135deg,#3B74F0,#2F6BEF);box-shadow:0 10px 22px -10px rgba(47,107,239,.7)}
.lp-btn.primary:hover{transform:translateY(-1px);box-shadow:0 16px 28px -12px rgba(47,107,239,.75);text-decoration:none}
.lp-btn.ghost{color:var(--text);border:1px solid var(--border-strong);background:#fff}
.lp-btn.ghost:hover{background:#f7f9fc;text-decoration:none;transform:translateY(-1px)}
.lp-wrap{max-width:1080px;margin:0 auto;padding:0 32px}
.lp-hero{text-align:center;padding:84px 0 20px}
.eyebrow{display:inline-flex;align-items:center;gap:7px;margin-bottom:22px;padding:6px 14px;border-radius:999px;
  font-size:12.5px;font-weight:650;letter-spacing:.2px;color:var(--accent-ink);background:rgba(47,107,239,.09);border:1px solid rgba(47,107,239,.16)}
.eyebrow .dot{background:var(--accent);box-shadow:0 0 0 3px rgba(47,107,239,.18)}
.lp-hero h1{font-size:52px;line-height:1.08;font-weight:820;letter-spacing:-1.6px;margin:0 auto 20px;max-width:800px}
.lp-hero h1 .grad{background:linear-gradient(120deg,#2F6BEF,#5b9dff);-webkit-background-clip:text;background-clip:text;color:transparent}
.lp-hero .lead{font-size:17.5px;line-height:1.6;color:var(--muted);max-width:600px;margin:0 auto 34px}
.lp-cta{display:flex;gap:14px;justify-content:center;flex-wrap:wrap}
.lp-cta .lp-btn{padding:13px 26px;font-size:15px}
.lp-trust{margin-top:22px;font-size:12.5px;color:var(--faint);display:flex;align-items:center;justify-content:center;gap:8px}
.lp-section{padding:56px 0}
.lp-kicker{text-align:center;font-size:12px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:var(--accent);margin:0 0 10px}
.lp-h2{text-align:center;font-size:30px;font-weight:780;letter-spacing:-.8px;margin:0 0 12px}
.lp-sub2{text-align:center;color:var(--muted);font-size:15px;max-width:560px;margin:0 auto 40px}
.lp-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:18px}
.fcard{background:#fff;border:1px solid var(--border);border-radius:16px;padding:26px 24px;
  box-shadow:0 20px 50px -34px rgba(15,23,42,.2);transition:transform .16s,box-shadow .16s}
.fcard:hover{transform:translateY(-3px);box-shadow:0 28px 60px -34px rgba(15,23,42,.28)}
.fcard .ic{width:42px;height:42px;border-radius:12px;display:grid;place-items:center;margin-bottom:16px;
  background:linear-gradient(135deg,rgba(47,107,239,.12),rgba(91,157,255,.10));border:1px solid rgba(47,107,239,.14)}
.fcard h3{font-size:16px;font-weight:700;margin:0 0 7px;letter-spacing:-.2px}
.fcard p{font-size:13.5px;line-height:1.55;color:var(--muted);margin:0}
.steps{display:grid;grid-template-columns:repeat(3,1fr);gap:18px;counter-reset:step}
.step{position:relative;background:#f8fafc;border:1px solid var(--border);border-radius:16px;padding:26px 24px}
.step::before{counter-increment:step;content:counter(step);display:grid;place-items:center;width:30px;height:30px;
  border-radius:9px;background:linear-gradient(135deg,#3B74F0,#2F6BEF);color:#fff;font-size:14px;font-weight:750;margin-bottom:14px}
.step h3{font-size:15.5px;font-weight:700;margin:0 0 6px}
.step p{font-size:13.5px;line-height:1.55;color:var(--muted);margin:0}
.lp-band{margin:56px 0 0;background:linear-gradient(135deg,#1e3a8a,#2F6BEF);border-radius:24px;padding:56px 40px;text-align:center;color:#fff}
.lp-band h2{font-size:30px;font-weight:800;letter-spacing:-.8px;margin:0 0 12px}
.lp-band p{font-size:15.5px;color:rgba(255,255,255,.86);max-width:520px;margin:0 auto 28px}
.lp-band .lp-btn.primary{background:#fff;color:var(--accent-ink);box-shadow:0 14px 30px -12px rgba(0,0,0,.4)}
.lp-band .lp-btn.primary:hover{background:#f2f6ff}
.lp-foot{max-width:1080px;margin:0 auto;padding:32px;display:flex;align-items:center;justify-content:space-between;gap:16px;
  border-top:1px solid var(--border);color:var(--muted);font-size:13px;flex-wrap:wrap;margin-top:40px}
.lp-foot .brand{margin:0}.lp-foot .wordmark{font-size:16px}

/* responsive: auth cards */
@media (max-width:480px){
  .center{padding:20px}
  .card{padding:30px 22px;border-radius:16px}
  h1{font-size:20px}
}
/* responsive: admin table collapses into stacked cards */
@media (max-width:640px){
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
/* responsive: landing */
@media (max-width:820px){
  .lp-grid,.steps{grid-template-columns:1fr}
  .lp-hero{padding:56px 0 16px}
  .lp-hero h1{font-size:38px;letter-spacing:-1px}
  .lp-hero .lead{font-size:16px}
  .lp-nav{padding:14px 20px}
  .lp-wrap{padding:0 20px}
  .lp-band{padding:44px 24px}
  .lp-foot{flex-direction:column;text-align:center}
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
    var m=(e&&e.name==='TimeoutError')?'Server took too long — try again.':'Network error — is the server running?';
    return {ok:false,status:0,data:{error:m}};
  }
}
</script>
{{block "script" .}}{{end}}
<div style="position:fixed;bottom:6px;right:9px;font-size:10px;color:#c2cad8;user-select:none">build 6</div>
</body></html>{{end}}`

const logoSVG = `<svg width="28" height="28" viewBox="0 0 100 100"><circle cx="50" cy="18" r="9" fill="#7CA0F2"/><circle cx="22" cy="34" r="8" fill="#C3D4F7"/><circle cx="78" cy="34" r="8" fill="#C3D4F7"/><circle cx="50" cy="50" r="13" fill="#2F6BEF"/><circle cx="22" cy="66" r="8" fill="#3B74F0"/><circle cx="78" cy="66" r="8" fill="#3B74F0"/><circle cx="50" cy="82" r="9" fill="#2F6BEF"/></svg>`

// --- home / landing ------------------------------------------------------

const homeBody = `{{define "body"}}
<nav class="lp-nav">
  <div class="brand">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span></span></div>
  <div class="lp-nav-actions">
    <a href="/login">Sign in</a>
    <a class="lp-btn primary" href="/signup">Get started</a>
  </div>
</nav>

<header class="lp-hero lp-wrap">
  <span class="eyebrow"><span class="dot"></span>Kubernetes · EKS · FinOps · AI</span>
  <h1>Your cluster's health &amp; cost,<br><span class="grad">in one calm view.</span></h1>
  <p class="lead">Clusterheart is a desktop dashboard for your Kubernetes clusters — live health, real AWS spend, right-sizing insights, and an AI copilot for kubectl. It reads your local kubeconfig, so credentials never leave your machine.</p>
  <div class="lp-cta">
    <a class="lp-btn primary" href="/signup">Create your account</a>
    <a class="lp-btn ghost" href="/login">Sign in</a>
  </div>
  <div class="lp-trust">
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
    Local &amp; secure — your cluster credentials never leave this machine.
  </div>
</header>

<section class="lp-section lp-wrap">
  <p class="lp-kicker">What it does</p>
  <h2 class="lp-h2">Everything you need to run a cluster, in one place</h2>
  <p class="lp-sub2">Health, cost and control — without juggling five terminals and three consoles.</p>
  <div class="lp-grid">
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg></span>
      <h3>Live cluster health</h3>
      <p>Pods, nodes, events and metrics updating in real time across your clusters.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="1" x2="12" y2="23"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg></span>
      <h3>FinOps cost insights</h3>
      <p>Real AWS daily spend from Cost Explorer, right beside your live usage.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/></svg></span>
      <h3>Right-sizing &amp; consolidation</h3>
      <p>Spot phantom requests and see how many nodes you could actually drop.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2a5 5 0 0 1 5 5c0 1.5-.5 2-1 3l-1 1v2H9v-2l-1-1c-.5-1-1-1.5-1-3a5 5 0 0 1 5-5Z"/><line x1="9" y1="18" x2="15" y2="18"/><line x1="10" y1="21" x2="14" y2="21"/></svg></span>
      <h3>AI copilot for kubectl</h3>
      <p>Ask in plain English; it runs read-only kubectl and explains what it finds.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg></span>
      <h3>Shell, logs &amp; port-forward</h3>
      <p>Exec into containers, stream logs and forward ports without leaving the app.</p>
    </div>
    <div class="fcard">
      <span class="ic"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#2F6BEF" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg></span>
      <h3>Local &amp; secure</h3>
      <p>Reads your local kubeconfig — cluster credentials never leave your machine.</p>
    </div>
  </div>
</section>

<section class="lp-section lp-wrap">
  <p class="lp-kicker">How it works</p>
  <h2 class="lp-h2">Up and running in three steps</h2>
  <div class="steps">
    <div class="step"><h3>Create your account</h3><p>Sign up in seconds. New accounts wait for a quick owner approval.</p></div>
    <div class="step"><h3>Get approved</h3><p>The owner approves you from the admin panel — you'll see it the moment they do.</p></div>
    <div class="step"><h3>Open the desktop app</h3><p>Launch Clusterheart, sign in, and your dashboard boots against your cluster.</p></div>
  </div>
</section>

<section class="lp-wrap">
  <div class="lp-band">
    <h2>Ready to see your cluster clearly?</h2>
    <p>Create your account and connect your cluster in minutes. No agents to install — it runs on your machine.</p>
    <a class="lp-btn primary" href="/signup">Get started free</a>
  </div>
</section>

<footer class="lp-foot">
  <div class="brand">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span></span></div>
  <div>Kubernetes health, cost &amp; control — on your machine.</div>
  <div><a href="/login">Sign in</a> · <a href="/signup">Create account</a></div>
</footer>
{{end}}`

// --- login ---------------------------------------------------------------

const loginBody = `{{define "body"}}<div class="center"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span></span></div>
<h1>Welcome back</h1><p class="sub">Sign in to your account.</p>
<label for="email">Email</label>
<input id="email" type="email" autocomplete="email" placeholder="you@company.com">
<label for="pass">Password</label>
<input id="pass" type="password" autocomplete="current-password" placeholder="••••••••">
<button class="btn" id="go">Sign in</button>
<div id="msg" class="msg"></div>
<div class="alt">New here? <a href="/signup">Create an account</a></div>
</div></div>{{end}}
{{define "script"}}<script>
var email=document.getElementById('email'),pass=document.getElementById('pass'),go=document.getElementById('go'),msg=document.getElementById('msg');
async function submit(){
  if(!email.value.trim()||!pass.value){_mark(msg,'Enter your email and password.','err');return;}
  go.disabled=true;_mark(msg,'Signing in…','info');
  var res=await _post('/api/login',{email:email.value.trim(),password:pass.value});
  if(!res.ok){_mark(msg,res.data.error||'Sign in failed.','err');go.disabled=false;return;}
  _mark(msg,'Welcome back — redirecting…','ok');location.href='/account';
}
go.addEventListener('click',submit);
pass.addEventListener('keydown',function(e){if(e.key==='Enter')submit();});
email.addEventListener('keydown',function(e){if(e.key==='Enter')pass.focus();});
</script>{{end}}`

// --- signup --------------------------------------------------------------

const signupBody = `{{define "body"}}<div class="center"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span></span></div>
<h1>Create your account</h1><p class="sub">Sign up to request access. An admin approves new accounts.</p>
<label for="name">Name</label>
<input id="name" type="text" autocomplete="name" placeholder="Jane Doe">
<label for="email">Work email</label>
<input id="email" type="email" autocomplete="email" placeholder="you@company.com">
<label for="pass">Password</label>
<input id="pass" type="password" autocomplete="new-password" placeholder="At least 8 characters">
<button class="btn" id="go">Create account</button>
<div id="msg" class="msg"></div>
<div class="alt">Already have an account? <a href="/login">Sign in</a></div>
</div></div>{{end}}
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

// --- account -------------------------------------------------------------

const accountBody = `{{define "body"}}<div class="center"><div class="card">
<div class="brand">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span></span></div>
<h1>Hi, {{.Name}}</h1><p class="sub">{{.Email}}</p>
{{if .Approved}}
  <p><span class="pill on"><span class="dot"></span>Approved</span></p>
  <p class="sub" style="margin-top:14px">You have access. Open the Clusterheart desktop app and sign in to start.</p>
{{else}}
  <p><span class="pill off"><span class="dot"></span>Pending approval</span></p>
  <p class="sub" style="margin-top:14px">Your account is waiting for an admin to approve it. You'll be able to use the app once approved.</p>
{{end}}
{{if .IsAdmin}}<div class="alt"><a href="/admin">Open admin panel →</a></div>{{end}}
<button class="btn secondary" id="logout">Log out</button>
</div></div>{{end}}
{{define "script"}}<script>
document.getElementById('logout').addEventListener('click',async function(){
  await _post('/api/logout',{});location.href='/login';
});
</script>{{end}}`

// --- admin ---------------------------------------------------------------

const adminBody = `{{define "body"}}<div class="admin">
<div class="admin-head">
  <div class="brand" style="margin:0">` + logoSVG + `<span class="wordmark">cluster<span class="a">heart</span> <span style="color:var(--muted);font-weight:500">· admin</span></span></div>
  <div class="admin-user">{{.Me}} · <a href="#" id="logout">log out</a></div>
</div>
<p class="sub" style="margin:0 0 22px">{{if .Pending}}<span class="count">{{.Pending}}</span>awaiting approval — approve people to grant access, revoke to cut it off.{{else}}Everyone's reviewed. Approve new sign-ups here, or revoke access anytime.{{end}}</p>
<div class="panel">
<table class="users">
<thead><tr><th>User</th><th>Status</th><th>Last login</th><th></th></tr></thead>
<tbody>
{{if not .Users}}<tr><td colspan="4" class="empty">No sign-ups yet. Share the /signup link.</td></tr>{{end}}
{{range .Users}}<tr>
  <td class="cell-user">
    <div class="u-name">{{.Name}}</div>
    <div class="u-email">{{.Email}}</div>
  </td>
  <td data-label="Status">
    {{if .Approved}}<span class="pill on"><span class="dot"></span>Approved</span>
    {{else}}<span class="pill off"><span class="dot"></span>Pending</span>{{end}}
  </td>
  <td data-label="Last login" class="u-time">
    {{if .LastLogin.IsZero}}never{{else}}{{.LastLogin.Format "Jan 2, 15:04"}}{{end}}
  </td>
  <td class="cell-act">
    {{if .Approved}}<button class="act revoke" data-email="{{.Email}}" data-approve="false">Revoke</button>
    {{else}}<button class="act approve" data-email="{{.Email}}" data-approve="true">Approve</button>{{end}}
  </td>
</tr>{{end}}
</tbody></table></div></div>{{end}}
{{define "script"}}<script>
document.querySelectorAll('.act').forEach(function(b){
  b.addEventListener('click',async function(){
    b.disabled=true;b.textContent='…';
    await _post('/api/admin/approve',{email:b.dataset.email,approved:b.dataset.approve==='true'});
    location.reload();
  });
});
document.getElementById('logout').addEventListener('click',async function(e){
  e.preventDefault();await _post('/api/logout',{});location.href='/login';
});
</script>{{end}}`
