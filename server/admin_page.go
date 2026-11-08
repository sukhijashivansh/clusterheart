package main

// The admin area is a sidebar shell with two separate pages, each its own
// route: Users (/admin) and Broadcast (/admin/broadcast). adminShell wraps a
// page's content + script in the shared sidebar chrome with the active nav lit.

// adminShell builds a full page template (body + script) around the sidebar.
// active is "users" or "broadcast"; content/pageScript are the page's markup
// and JS (a shared logout handler is always included).
func adminShell(active, content, pageScript string) string {
	navItem := func(key, href, label string) string {
		cls := "nav-item"
		if key == active {
			cls += " active"
		}
		return `<a class="` + cls + `" href="` + href + `">` + label + `</a>`
	}
	return `{{define "body"}}<div class="admin-shell">
<aside class="admin-nav">
  <div class="an-brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
  <nav class="an-list">` +
		navItem("users", "/admin", "Users") +
		navItem("broadcast", "/admin/broadcast", "Broadcast") +
		`</nav>
  <div class="an-foot"><div class="an-me">{{.Me}}</div><a href="#" id="logout" class="an-logout">Log out</a></div>
</aside>
<main class="admin-main">` + content + `</main>
</div>{{end}}
{{define "script"}}<script>
var _lo=document.getElementById('logout');
if(_lo)_lo.addEventListener('click',async function(e){e.preventDefault();await _post('/api/logout',{});location.href='/login';});
` + pageScript + `
</script>{{end}}`
}

// --- Users page ----------------------------------------------------------

const usersContent = `
<p class="kicker">Access control</p>
{{if .Pending}}<p class="sub"><span class="count">{{.Pending}}</span>awaiting approval. Approve to grant access, revoke to cut it off.</p>{{end}}
<div class="adm-totals"><div><b>{{len .Users}}</b><span>Users</span></div><div><b>{{.TotalClusters}}</b><span>Clusters</span></div><div><b>{{.TotalServers}}</b><span>Servers</span></div></div>
<section class="card2" style="margin-top:16px">
<table class="users">
<thead><tr><th>User</th><th>Status</th><th>Clusters</th><th>Servers</th><th>Team</th><th>Last seen</th><th></th></tr></thead>
<tbody>
{{if not .Users}}<tr><td colspan="7" class="empty">No sign-ups yet. Share the /signup link.</td></tr>{{end}}
{{range .Users}}<tr>
  <td class="cell-user"><div class="u-name">{{.Name}}</div><div class="u-email">{{.Email}}</div></td>
  <td data-label="Status">{{if .Approved}}<span class="pill on"><span class="dot"></span>Approved</span>{{else}}<span class="pill off"><span class="dot"></span>Pending</span>{{end}}</td>
  <td data-label="Clusters" class="u-cnt">{{if not .Known}}<span class="u-none">&mdash;</span>{{else if .Clusters}}<b>{{.Clusters}}</b>{{if lt .ClustersOn .Clusters}} <span class="u-off">{{.ClustersOn}} live</span>{{end}}{{else}}<span class="u-none">0</span>{{end}}</td>
  <td data-label="Servers" class="u-cnt">{{if not .Known}}<span class="u-none">&mdash;</span>{{else if .Servers}}<b>{{.Servers}}</b>{{if lt .ServersOn .Servers}} <span class="u-off">{{.ServersOn}} live</span>{{end}}{{else}}<span class="u-none">0</span>{{end}}</td>
  <td data-label="Team" class="u-cnt">{{if .TeamSize}}<b>{{.TeamSize}}</b> <span class="u-none">{{if eq .TeamSize 1}}person{{else}}people{{end}}</span>{{end}}{{range .MemberOf}}<div class="u-in">in {{.}}</div>{{end}}{{if and (not .TeamSize) (not .MemberOf)}}<span class="u-none">&mdash;</span>{{end}}</td>
  <td data-label="Last seen" class="u-time">{{if .LastSeen.IsZero}}{{if .LastLogin.IsZero}}never{{else}}{{.LastLogin.Format "Jan 2, 15:04"}}{{end}}{{else}}{{.LastSeen.Format "Jan 2, 15:04"}}{{end}}</td>
  <td class="cell-act">{{if .Approved}}<button class="act forceout" data-email="{{.Email}}">Force out</button><button class="act revoke" data-email="{{.Email}}" data-approve="false">Revoke</button>{{else}}<button class="act approve" data-email="{{.Email}}" data-approve="true">Approve</button>{{end}}</td>
</tr>{{end}}
</tbody></table>
</section>`

const usersScript = `
document.querySelectorAll('.act[data-approve]').forEach(function(b){
  b.addEventListener('click',async function(){
    b.disabled=true;b.textContent='…';
    await _post('/api/admin/approve',{email:b.dataset.email,approved:b.dataset.approve==='true'});
    location.reload();
  });
});
document.querySelectorAll('.act.forceout').forEach(function(b){
  b.addEventListener('click',async function(){
    b.disabled=true;b.textContent='…';
    await _post('/api/admin/forcelogout',{email:b.dataset.email});
    location.reload();
  });
});`

// --- Broadcast page ------------------------------------------------------

const broadcastContent = `
<p class="kicker">Access control</p>
<h1 style="margin:0 0 7px">Broadcast</h1>
<p class="sub">Show a short message at the top of everyone's dashboard. Clear it to take it down.</p>
<section class="card2" style="margin-top:24px"><div class="card2-body">
  <textarea id="notice-text" class="nb" rows="3" placeholder="e.g. Scheduled maintenance tonight at 22:00 UTC.">{{.Notice}}</textarea>
  <div class="nb-actions">
    <button class="act approve" id="notice-send">Send to everyone</button>
    <button class="act revoke" id="notice-clear">Clear</button>
    <span id="notice-msg" class="nb-msg"></span>
  </div>
</div></section>`

const broadcastScript = `
(function(){
  var send=document.getElementById('notice-send'),clr=document.getElementById('notice-clear'),
      txt=document.getElementById('notice-text'),msg=document.getElementById('notice-msg');
  async function put(message,label){
    send.disabled=clr.disabled=true;_mark(msg,label,'info');
    var r=await _post('/api/admin/notice',{message:message});
    send.disabled=clr.disabled=false;
    _mark(msg,r.ok?'Saved.':(r.data.error||'Failed.'),r.ok?'ok':'err');
  }
  send.addEventListener('click',function(){put(txt.value,'Sending…');});
  clr.addEventListener('click',function(){txt.value='';put('','Clearing…');});
})();`

var (
	adminUsersTmpl     = page(adminShell("users", usersContent, usersScript))
	adminBroadcastTmpl = page(adminShell("broadcast", broadcastContent, broadcastScript))
)
