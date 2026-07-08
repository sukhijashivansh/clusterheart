const $ = (id) => document.getElementById(id);
const views = { loading: $('loading'), form: $('form'), pending: $('pending') };

function show(name) {
  for (const k in views) views[k].classList.toggle('hidden', k !== name);
}
function say(text, kind) {
  const m = $('msg');
  m.textContent = text;
  m.className = 'msg ' + (kind || 'info');
}

async function boot() {
  const r = await window.clusterheart.start();
  if (!r.ok) {
    show('form');
    say('Could not start the dashboard: ' + r.error, 'err');
  }
  // On success the window navigates to the dashboard; nothing else to do.
}

async function attemptLogin(email, password) {
  const res = await window.clusterheart.login(email, password);
  if (!res.ok) {
    show('form');
    say(res.error || 'Sign in failed.', 'err');
    return;
  }
  if (!res.approved) {
    $('pendingName').textContent =
      (res.name ? res.name + ', your' : 'Your') +
      " account is waiting for the owner to approve it. You'll get in as soon as they do.";
    show('pending');
    return;
  }
  say('Approved — starting…', 'ok');
  await boot();
}

// --- form wiring ---------------------------------------------------------
$('go').addEventListener('click', async () => {
  const email = $('email').value.trim();
  const pass = $('pass').value;
  if (!email || !pass) { say('Enter your email and password.', 'err'); return; }
  $('go').disabled = true;
  say('Signing in…', 'info');
  await attemptLogin(email, pass);
  $('go').disabled = false;
});
$('pass').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('go').click(); });
$('email').addEventListener('keydown', (e) => { if (e.key === 'Enter') $('pass').focus(); });
$('signup').addEventListener('click', () => window.clusterheart.openSignup());

// --- pending view wiring -------------------------------------------------
$('recheck').addEventListener('click', async () => {
  $('recheck').disabled = true;
  const r = await window.clusterheart.resume();
  $('recheck').disabled = false;
  if (r.approved) { await boot(); return; }
  // still pending — leave the view as-is
});
$('signout').addEventListener('click', async () => {
  await window.clusterheart.logout(); // clears token, reloads login.html
});

// --- startup: try to resume an approved session --------------------------
(async () => {
  const r = await window.clusterheart.resume();
  if (r.approved) { await boot(); return; }
  if (r.pending) {
    $('pendingName').textContent =
      (r.name ? r.name + ', your' : 'Your') +
      " account is waiting for the owner to approve it. You'll get in as soon as they do.";
    show('pending');
    return;
  }
  show('form');
})();
