function getCSRFToken() {
  var meta = document.querySelector('meta[name="csrf-token"]');
  return meta ? meta.content : '';
}

// Nothing animates; htmx would smooth-scroll a boosted swap to the top.
if (window.htmx) window.htmx.config.scrollBehavior = 'auto';

document.body.addEventListener('htmx:configRequest', function(e) {
  e.detail.headers['X-CSRF-Token'] = getCSRFToken();
});

function escapeHTML(s) {
  var d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

// textContent, so a name in the text cannot inject markup.
function setFlashText(slot, kind, text) {
  if (!slot) return;
  var d = document.createElement('div');
  d.className = 'flash flash-' + kind;
  d.textContent = text;
  slot.textContent = '';
  slot.appendChild(d);
}

function writeStash(key, obj) {
  obj.t = Date.now();
  try { sessionStorage.setItem(key, JSON.stringify(obj)); } catch (e) {}
}

// null when absent, unreadable or older than maxAgeMs; take removes it.
function readStash(key, maxAgeMs, take) {
  var raw = null;
  try {
    raw = sessionStorage.getItem(key);
    if (raw && take) sessionStorage.removeItem(key);
  } catch (e) {}
  var stash = null;
  try { stash = JSON.parse(raw); } catch (e) {}
  if (!stash || Date.now() - stash.t > maxAgeMs) return null;
  return stash;
}

function showActionFlash(html, kind) {
  var slot = document.querySelector('[data-action-flash]');
  if (!slot || !html) return;
  var cls = kind === 'err' ? 'flash-err' : 'flash-ok';
  if (/class\s*=\s*"[^"]*flash\b/.test(html)) {
    slot.innerHTML = html;
  } else {
    slot.innerHTML = '<div class="flash ' + cls + '">' + html + '</div>';
  }
  var token = String(Date.now()) + Math.random();
  slot.dataset.token = token;
  if (kind === 'err' || /flash-(err|warn)/.test(slot.innerHTML)) return;
  setTimeout(function() {
    if (slot.dataset.token === token) slot.innerHTML = '';
  }, 5000);
}

function stashActionFlash(html, kind) {
  if (html) writeStash('action_flash', {h: html, k: kind || 'ok'});
}

// Covers an action's own reload, not a later visit after an action that
// never navigated.
var stashStalenessMs = 10000;

document.addEventListener('DOMContentLoaded', function() {
  if (!document.querySelector('[data-action-flash]')) return;
  var stash = readStash('action_flash', stashStalenessMs, true);
  if (stash && stash.h) showActionFlash(stash.h, stash.k);
});

// Shown and stashed both: the same response may or may not end in a
// redirect or refresh.
document.body.addEventListener('action-flash', function(e) {
  if (!e || !e.detail) return;
  var text = e.detail.text || '';
  var kind = e.detail.kind || 'ok';
  showActionFlash(text, kind);
  stashActionFlash(text, kind);
});

var _chordNode = null;
var _chordLabel = '';
var _chordTimer = null;
var _chordHint = null;
var chordTimeoutMs = 500;

function clearChord() {
  _chordNode = null;
  _chordLabel = '';
  if (_chordTimer) { clearTimeout(_chordTimer); _chordTimer = null; }
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = null;
}

function enterChord(node, label) {
  _chordNode = node;
  _chordLabel = label;
  showChordHint(node, label);
  if (_chordTimer) clearTimeout(_chordTimer);
  _chordTimer = setTimeout(clearChord, chordTimeoutMs);
}

function showChordHint(node, label) {
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = document.createElement('div');
  _chordHint.className = 'chord-leader-hint';
  var keys = Object.keys(node || {}).filter(function(k) {
    var leaf = node[k];
    return !Array.isArray(leaf) || leaf.some(function(sel) { return document.querySelector(sel); });
  });
  var digits = typeof chordDigitTargets === 'function' ? chordDigitTargets(node) : null;
  if (digits) {
    for (var i = 1; i <= digits.length && i <= 9; i++) keys.push(String(i));
  }
  keys.sort();
  _chordHint.textContent = label + ' → ' + keys.join(' / ');
  document.body.appendChild(_chordHint);
}

// Takes the key while a chord is open; chordDigitTargets, when defined,
// numbers a node's buttons. An unknown key returns false so it still does
// its single-key job.
function chordStep(e) {
  if (!_chordNode) return false;
  var node = _chordNode;
  var label = _chordLabel;
  var next = node[e.key];
  if (next && typeof next === 'object' && !Array.isArray(next)) {
    e.preventDefault();
    enterChord(next, label + ' ' + e.key);
    return true;
  }
  clearChord();
  if (next !== undefined) {
    e.preventDefault();
    if (typeof next === 'string') window.location.href = next;
    else if (Array.isArray(next)) clickFirstSelector(next);
    else next();
    return true;
  }
  var digitTargets = typeof chordDigitTargets === 'function' ? chordDigitTargets(node) : null;
  if (digitTargets && /^[1-9]$/.test(e.key)) {
    var picked = digitTargets[parseInt(e.key, 10) - 1];
    if (picked) { e.preventDefault(); picked.click(); return true; }
  }
  return false;
}

// The chords the page declares: data-chord="g q" on an element clicks it.
function chordMapFromPage() {
  var map = {};
  document.querySelectorAll('[data-chord]').forEach(function(el) {
    var keys = el.dataset.chord.split(' ');
    var node = map;
    keys.slice(0, -1).forEach(function(k) { node = node[k] = node[k] || {}; });
    node[keys[keys.length - 1]] = function() { el.click(); };
  });
  return map;
}

function clickFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { el.click(); return true; }
  }
  return false;
}

// A row's data-when must be the selector its key's handler looks for;
// activeSurfaces, when defined, names the sections that apply here.
function syncShortcutSheet(dlg) {
  var all = dlg.classList.contains('show-all');
  var active = typeof activeSurfaces === 'function' ? activeSurfaces() : ['anywhere'];
  dlg.querySelectorAll('section[data-surface]').forEach(function(sec) {
    sec.hidden = !all && active.indexOf(sec.dataset.surface) === -1;
  });
  dlg.querySelectorAll('[data-when]').forEach(function(row) {
    row.hidden = !all && !document.querySelector(row.dataset.when);
  });
}

function openShortcutSheet() {
  var dlg = document.getElementById('shortcuts-help');
  if (!dlg) return false;
  dlg.classList.remove('show-all');
  var toggle = document.getElementById('shortcuts-all');
  if (toggle) toggle.textContent = '[all surfaces]';
  syncShortcutSheet(dlg);
  if (!dlg.open) dlg.showModal();
  // showModal scrolls to the first focusable element, which on a short
  // viewport hides the title.
  dlg.scrollTop = 0;
  return true;
}

document.addEventListener('click', function(e) {
  if (!e.target.closest || !e.target.closest('#shortcuts-all')) return;
  var dlg = document.getElementById('shortcuts-help');
  var all = dlg.classList.toggle('show-all');
  e.target.textContent = all ? '[this page]' : '[all surfaces]';
  syncShortcutSheet(dlg);
});

function showPageNotice(text) {
  const el = document.getElementById('page-notice');
  if (!el) return null;
  el.textContent = text;
  delete el.dataset.session;
  el.hidden = false;
  return el;
}

function loginURL() {
  const el = document.getElementById('page-notice');
  return (el && el.dataset.loginUrl) || '/login';
}

// fetch follows the login redirect a write without a session gets, and
// reports the login page as a success.
function sessionLost(res) {
  return res.status === 401 || (res.redirected && new URL(res.url).pathname === loginURL());
}

function showLoginNotice() {
  const el = showPageNotice('You are logged out: ');
  if (!el) return;
  const a = document.createElement('a');
  a.href = loginURL();
  a.target = '_blank';
  a.textContent = '[log in]';
  el.append(a, ' in another tab, then retry here. This page keeps what you typed.');
  el.dataset.session = '1';
}

document.body.addEventListener('htmx:beforeSwap', function(e) {
  const xhr = e.detail.xhr;
  if (!xhr) return;
  if (xhr.status === 401) {
    showLoginNotice();
    return;
  }
  if (xhr.status === 403) {
    showPageNotice(xhr.responseText);
    return;
  }
  const notice = document.getElementById('page-notice');
  const saved = xhr.status < 400 && e.detail.requestConfig && e.detail.requestConfig.verb !== 'get';
  if (saved && notice && notice.dataset.session) notice.hidden = true;
});

// A GET saves nothing, and a poll fails on every tick of an outage.
document.body.addEventListener('htmx:sendError', function(e) {
  if (e.detail.requestConfig && e.detail.requestConfig.verb === 'get') return;
  [e.detail.target].concat(Array.from(document.querySelectorAll('[data-action-flash]'))).forEach(function(slot) {
    if (slot) slot.querySelectorAll('.flash-ok').forEach(function(f) { f.remove(); });
  });
  showUnansweredNotice();
});

function showUnansweredNotice() {
  const el = showPageNotice('The server did not answer: nothing was saved. Retry once it is back.');
  if (el) el.dataset.session = '1';
}

function confirmOpen() {
  var dlg = document.getElementById('confirm-dialog');
  return !!(dlg && dlg.open);
}

// alt is an optional {label, run} third choice beside OK and Cancel.
function showConfirm(message, onOk, danger, okLabel, alt, focusCancel) {
  var dlg = document.getElementById('confirm-dialog');
  if (!dlg) { if (window.confirm(message)) onOk(); return; }
  document.getElementById('confirm-dialog-msg').textContent = message || '';
  document.getElementById('confirm-dialog-danger').textContent = danger || '';
  var okBtn = document.getElementById('confirm-dialog-ok');
  var cancelBtn = document.getElementById('confirm-dialog-cancel');
  var altBtn = document.getElementById('confirm-dialog-alt');
  okBtn.textContent = okLabel || 'OK';
  altBtn.hidden = !alt;
  altBtn.textContent = alt ? alt.label : '';
  var close = function() { dlg.close(); okBtn.onclick = null; cancelBtn.onclick = null; altBtn.onclick = null; };
  okBtn.onclick = function() { close(); onOk(); };
  altBtn.onclick = alt ? function() { close(); alt.run(); } : null;
  cancelBtn.onclick = close;
  dlg.showModal();
  if (focusCancel === undefined ? danger : focusCancel) cancelBtn.focus(); else okBtn.focus();
}

document.body.addEventListener('htmx:confirm', function(e) {
  if (!e.detail || !e.detail.question) return;
  e.preventDefault();
  var elt = e.detail.elt;
  var ds = elt && elt.dataset ? elt.dataset : {};
  var alt = null;
  if (ds.confirmAlt && ds.confirmAltUrl) {
    // The form's hidden fields carry the origin's identity.
    alt = {label: ds.confirmAlt, run: function() {
      var values = {};
      var form = elt.closest('form');
      if (form) {
        form.querySelectorAll('input[type="hidden"]').forEach(function(i) { values[i.name] = i.value; });
      }
      if (ds.confirmAltValue) {
        values[ds.confirmAltValue.split('=')[0]] = ds.confirmAltValue.split('=')[1];
      }
      // No source element, or htmx inherits the hx-confirm and asks the
      // same question again.
      var target = ds.confirmAltTarget ? document.querySelector(ds.confirmAltTarget) : document.querySelector('[data-action-flash]');
      htmx.ajax('POST', ds.confirmAltUrl, {values: values, target: target});
    }};
  }
  var focusCancel = !!(elt && elt.hasAttribute && elt.hasAttribute('data-confirm-danger'));
  showConfirm(e.detail.question, function() { e.detail.issueRequest(true); }, ds.confirmDanger, ds.confirmOk, alt, focusCancel);
});

// htmx:confirm only fires for htmx requests, so hx-confirm on a plain form is
// handled here; form.submit() fires no submit event, so it cannot loop.
var htmxVerbs = ['hx-get', 'hx-post', 'hx-put', 'hx-patch', 'hx-delete'];
document.body.addEventListener('submit', function(e) {
  var form = e.target;
  if (!form.hasAttribute || !form.hasAttribute('hx-confirm')) return;
  if (htmxVerbs.some(function(v) { return form.hasAttribute(v); })) return;
  e.preventDefault();
  var ds = form.dataset;
  showConfirm(form.getAttribute('hx-confirm'), function() { form.submit(); }, ds.confirmDanger, ds.confirmOk, null, form.hasAttribute('data-confirm-danger'));
});

function closeDialogFromSaveEvent(e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
}
document.body.addEventListener('token-saved', closeDialogFromSaveEvent);

// The execCommand fallback is for plain-HTTP LAN installs, which have no
// secure context and so no navigator.clipboard.
function copyToClipboard(text, flashEl) {
  var done = function () {
    if (!flashEl) return;
    flashEl.hidden = false;
    setTimeout(function () { flashEl.hidden = true; }, 1500);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () {
      copyToClipboardLegacy(text, done);
    });
    return;
  }
  copyToClipboardLegacy(text, done);
}

function copyToClipboardLegacy(text, done) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (e) { /* fall through */ }
  document.body.removeChild(ta);
  done();
}

document.addEventListener('click', function (e) {
  var btn = e.target.closest('[data-copy], [data-copy-from]');
  if (!btn) return;
  e.preventDefault();
  var text = btn.dataset.copy;
  if (!text && btn.dataset.copyFrom) {
    var src = document.querySelector(btn.dataset.copyFrom);
    text = src ? (src.value !== undefined ? src.value : src.textContent) : '';
  }
  var flash = btn.parentElement && btn.parentElement.querySelector('.copy-flash');
  copyToClipboard(text, flash);
});

// Browse only opens the dialog; the button's own hx-get fetches the listing.
document.addEventListener('click', function (e) {
  if (e.target.closest('.dir-picker-open')) {
    var dlg = document.getElementById('dir-picker-dialog');
    if (dlg && !dlg.open) dlg.showModal();
    return;
  }
  var use = e.target.closest('.dir-picker-use');
  if (!use) return;
  var input = document.getElementById(use.dataset.into || '');
  if (input) {
    input.value = use.dataset.path || '';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  var open = use.closest('dialog');
  if (open) open.close();
});

document.body.addEventListener('click', function(e) {
  var tab = e.target.closest && e.target.closest('.dialog-tab');
  if (!tab || !tab.dataset.panel) return;
  var strip = tab.closest('.dialog-tabs');
  var scope = strip && strip.parentElement;
  if (!scope) return;
  strip.querySelectorAll('.dialog-tab').forEach(function(t) {
    t.classList.toggle('active', t === tab);
  });
  scope.querySelectorAll('.dialog-panel').forEach(function(p) {
    p.hidden = p.id !== tab.dataset.panel;
  });
});

// A light without data-peer-off says nothing about the peer.
function applyPeerGating(scope) {
  const light = document.getElementById('peer-light');
  if (!light || !light.hasAttribute('data-peer-off')) return;
  const off = light.dataset.peerOff || '';
  const gate = function(el) {
    el.disabled = off !== '';
    const title = off || el.dataset.peerTitle;
    if (title) el.title = title;
    else el.removeAttribute('title');
  };
  if (scope.matches && scope.matches('[data-peer-title]')) gate(scope);
  scope.querySelectorAll('[data-peer-title]').forEach(gate);
}
applyPeerGating(document);

// History restore swaps with neither target nor xhr.
document.body.addEventListener('htmx:afterSwap', function(e) {
  const target = e.detail && e.detail.target;
  if (!target) return;
  applyPeerGating(target.id === 'peer-light' || !target.isConnected ? document : target);
});
