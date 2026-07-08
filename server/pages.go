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
  --bg:#070910;--panel:#0e1420;--panel2:#121a29;--text:#e8ecf5;--muted:#8b95a9;
  --border:rgba(255,255,255,.08);--accent:#2F6BEF;--accent2:#5b9dff;
  --green:#3ecf8e;--amber:#f5b544;--red:#f2555a;
}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%}
body{
  background:var(--bg);color:var(--text);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Inter,Roboto,sans-serif;
  -webkit-font-smoothing:antialiased;line-height:1.5;
  background-image:
    radial-gradient(600px 400px at 15% -5%,rgba(47,107,239,.22),transparent 60%),
    radial-gradient(700px 500px at 110% 110%,rgba(91,157,255,.14),transparent 55%);
  background-attachment:fixed;
}
a{color:var(--accent2);text-decoration:none}a:hover{text-decoration:underline}
.center{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:32px}
.card{
  width:100%;max-width:400px;padding:40px 36px;
  background:linear-gradient(180deg,rgba(255,255,255,.035),rgba(255,255,255,.012));
  border:1px solid var(--border);border-radius:18px;
  box-shadow:0 30px 80px -20px rgba(0,0,0,.7),0 0 0 1px rgba(255,255,255,.02) inset;
}
.brand{display:flex;align-items:center;gap:11px;margin-bottom:22px}
.brand svg{filter:drop-shadow(0 4px 10px rgba(47,107,239,.5))}
.wordmark{font-size:20px;font-weight:700;letter-spacing:-.4px}
.wordmark .a{color:var(--accent2)}
h1{font-size:22px;font-weight:700;letter-spacing:-.5px;margin:0 0 6px}
.sub{color:var(--muted);font-size:13.5px;margin:0 0 26px}
label{display:block;font-size:12px;font-weight:500;color:var(--muted);margin:16px 0 7px}
input{
  width:100%;padding:12px 14px;font-size:14px;color:var(--text);
  background:rgba(255,255,255,.04);border:1px solid var(--border);border-radius:10px;outline:none;
  transition:border-color .15s,box-shadow .15s;
}
input:focus{border-color:var(--accent);box-shadow:0 0 0 3px rgba(47,107,239,.22)}
input::placeholder{color:#586074}
.btn{
  width:100%;margin-top:24px;padding:12px 16px;font-size:14px;font-weight:600;color:#fff;cursor:pointer;
  background:linear-gradient(135deg,#3B74F0,#5b9dff);border:none;border-radius:10px;
  box-shadow:0 8px 22px -6px rgba(47,107,239,.6);transition:transform .12s,box-shadow .12s,opacity .12s;
}
.btn:hover{transform:translateY(-1px);box-shadow:0 12px 26px -6px rgba(47,107,239,.7)}
.btn:active{transform:translateY(0)}
.btn:disabled{opacity:.55;cursor:default;transform:none;box-shadow:none}
.alt{margin-top:20px;font-size:13px;color:var(--muted);text-align:center}
.msg{min-height:20px;margin-top:16px;font-size:13px}
.msg.err{color:var(--red)}.msg.ok{color:var(--green)}.msg.info{color:var(--muted)}
.pill{display:inline-flex;align-items:center;gap:6px;padding:4px 11px;border-radius:20px;font-size:12px;font-weight:600}
.pill.on{background:rgba(62,207,142,.14);color:var(--green)}
.pill.off{background:rgba(245,181,68,.14);color:var(--amber)}
.dot{width:6px;height:6px;border-radius:50%;background:currentColor}

/* --- admin panel --- */
.admin{max-width:880px;margin:0 auto;padding:52px 24px}
.admin-head{display:flex;align-items:center;justify-content:space-between;gap:16px;margin-bottom:6px}
.admin-user{font-size:13px;color:var(--muted);white-space:nowrap}
.count{display:inline-flex;align-items:center;justify-content:center;min-width:20px;height:20px;padding:0 7px;margin-right:8px;border-radius:20px;background:rgba(245,181,68,.16);color:var(--amber);font-size:12px;font-weight:700;vertical-align:middle}
.panel{background:linear-gradient(180deg,rgba(255,255,255,.035),rgba(255,255,255,.01));border:1px solid var(--border);border-radius:16px;overflow:hidden;box-shadow:0 30px 70px -34px rgba(0,0,0,.8)}
table.users{width:100%;border-collapse:collapse}
table.users th{text-align:left;padding:13px 18px;font-size:11px;text-transform:uppercase;letter-spacing:.6px;color:var(--muted);font-weight:600;background:rgba(255,255,255,.02);border-bottom:1px solid var(--border)}
table.users td{padding:15px 18px;vertical-align:middle}
table.users tbody tr{border-top:1px solid var(--border);transition:background .12s}
table.users tbody tr:hover{background:rgba(255,255,255,.025)}
.u-name{font-weight:600;font-size:14px}
.u-email{font-family:ui-monospace,SFMono-Regular,monospace;font-size:12px;color:var(--muted);margin-top:2px}
.u-time{font-size:13px;color:var(--muted)}
.cell-act{text-align:right;white-space:nowrap}
.act{padding:7px 16px;border-radius:8px;font-size:12.5px;font-weight:600;cursor:pointer;transition:transform .12s,opacity .12s,box-shadow .12s,border-color .12s,color .12s}
.act:hover{transform:translateY(-1px)}
.act:active{transform:translateY(0)}
.act:disabled{opacity:.55;cursor:default;transform:none;box-shadow:none}
.act.approve{border:none;color:#04140c;background:linear-gradient(135deg,#2fae6e,#3ecf8e);box-shadow:0 8px 20px -8px rgba(62,207,142,.7)}
.act.revoke{border:1px solid var(--border);background:transparent;color:var(--muted)}
.act.revoke:hover{color:var(--text);border-color:rgba(255,255,255,.22)}
.empty{padding:40px 18px;text-align:center;color:var(--muted)}

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
<div style="position:fixed;bottom:6px;right:9px;font-size:10px;color:#3a4150;user-select:none">build 5</div>
</body></html>{{end}}`

const logoSVG = `<svg width="28" height="28" viewBox="0 0 100 100"><circle cx="50" cy="18" r="9" fill="#7CA0F2"/><circle cx="22" cy="34" r="8" fill="#C3D4F7"/><circle cx="78" cy="34" r="8" fill="#C3D4F7"/><circle cx="50" cy="50" r="13" fill="#2F6BEF"/><circle cx="22" cy="66" r="8" fill="#3B74F0"/><circle cx="78" cy="66" r="8" fill="#3B74F0"/><circle cx="50" cy="82" r="9" fill="#2F6BEF"/></svg>`

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
<button class="btn" id="logout" style="background:linear-gradient(135deg,#39414f,#4a5464);box-shadow:none">Log out</button>
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
