document.addEventListener("DOMContentLoaded", function () {
  var bar = document.getElementById("add-input");
  if (bar && !matchMedia("(pointer: coarse)").matches) {
    bar.focus();
  }
});

// A form reset after an add puts the server's default back, so the stored
// pick is re-applied once the reset is done.
function restoreItemsPick() {
  var pick;
  try {
    pick = localStorage.getItem("monloaderItemsPick");
  } catch (e) {
    return;
  }
  if (pick === null) {
    return;
  }
  document.querySelectorAll(".add-row select[name=items]").forEach(function (sel) {
    if (Array.from(sel.options).some(function (o) { return o.value === pick; })) {
      sel.value = pick;
    }
  });
}
document.addEventListener("DOMContentLoaded", restoreItemsPick);

// The full prompt is cut off at this width, where the field takes a line of its own.
document.addEventListener("DOMContentLoaded", function () {
  if (matchMedia("(max-width: 600px)").matches) {
    document.querySelectorAll("input[data-short-placeholder]").forEach(function (input) {
      input.placeholder = input.dataset.shortPlaceholder;
    });
  }
});
document.addEventListener("reset", function () { setTimeout(restoreItemsPick, 0); }, true);
document.addEventListener("change", function (e) {
  if (e.target.matches(".add-row select[name=items]")) {
    try {
      localStorage.setItem("monloaderItemsPick", e.target.value);
    } catch (err) {}
  }
});

// A rows swap re-renders each job open and its "+N more" closed, so the
// operator's toggles are re-applied after it. toggle does not bubble, hence
// the capture; sessionStorage keeps collapsed rows across a reload.
function loadClosedJobs() {
  try {
    return new Set(JSON.parse(sessionStorage.getItem("monloaderClosedJobs") || "[]"));
  } catch (e) {
    return new Set();
  }
}
var closedJobs = loadClosedJobs();
var expandedMore = new Set();
function expandedMoreRoots() {
  return Array.from(expandedMore).join(",");
}
function applyJobStates() {
  closedJobs.forEach(function (id) {
    var d = document.querySelector('#queue-rows details[data-job="' + id + '"]');
    if (d) {
      d.open = false;
    }
  });
  expandedMore.forEach(function (id) {
    var d = document.querySelector('#queue-rows details[data-job="' + id + '"] details.more-items');
    if (d) {
      d.open = true;
    }
  });
}
document.addEventListener("toggle", function (e) {
  var d = e.target;
  if (!d || d.tagName !== "DETAILS") {
    return;
  }
  if (d.dataset.job) {
    if (d.open) {
      closedJobs.delete(d.dataset.job);
    } else {
      closedJobs.add(d.dataset.job);
    }
    try {
      sessionStorage.setItem("monloaderClosedJobs", JSON.stringify(Array.from(closedJobs)));
    } catch (e) {}
  } else if (d.classList.contains("more-items")) {
    var owner = d.closest("details[data-job]");
    if (owner) {
      if (d.open) {
        expandedMore.add(owner.dataset.job);
        // A live row's poll sends these items only once this is open.
        var rows = document.getElementById("queue-rows");
        var body = d.querySelector(".job-items");
        if (rows && body && !body.children.length) {
          htmx.trigger(rows, "more-opened");
        }
      } else {
        expandedMore.delete(owner.dataset.job);
      }
    }
  }
}, true);
document.addEventListener("DOMContentLoaded", applyJobStates);
document.body.addEventListener("htmx:afterSwap", function (e) {
  if (e.target && (e.target.id === "queue-rows" || e.target.classList.contains("job-items"))) {
    applyJobStates();
  }
});

// A swap collapses a text selection, and a row's controls have no id for htmx
// to give focus back to. :focus-visible, so a mouse click on a row does not
// freeze its own progress.
function pollHeld(sel) {
  if (confirmOpen()) {
    return true;
  }
  var box = document.querySelector(sel);
  if (!box) {
    return false;
  }
  var picked = window.getSelection();
  if (picked && !picked.isCollapsed && box.contains(picked.anchorNode)) {
    return true;
  }
  return box.querySelector(":focus-visible") !== null;
}

var bannerSteps = {
  unconfigured: ["/settings#monbooru", "Set its address in Settings"],
  unpaired: ["/settings#pairing", "Pair it in Settings"],
  rejected: ["/settings#pairing", "Pair again in Settings"],
  down: ["/settings#monbooru", "Check the address in Settings"],
};

function applyMonbooruBanner() {
  var light = document.getElementById("peer-light");
  var banner = document.getElementById("monbooru-banner");
  if (!light || !banner || light.dataset.conn === "checking") {
    return;
  }
  var off = light.dataset.peerOff || "";
  banner.hidden = off === "";
  var msg = document.getElementById("monbooru-banner-msg");
  if (msg && off) {
    msg.textContent = off;
  }
  // A pause is the operator's own doing, so the banner points at the
  // light, not the settings.
  var fix = document.getElementById("monbooru-banner-fix");
  if (fix) {
    fix.hidden = light.dataset.conn === "paused";
    var step = bannerSteps[light.dataset.conn] || bannerSteps.down;
    var link = fix.querySelector("a");
    link.href = step[0];
    link.textContent = step[1];
  }
}
document.addEventListener("DOMContentLoaded", applyMonbooruBanner);

document.addEventListener("DOMContentLoaded", function () {
  var url = document.body.dataset.canonicalUrl;
  if (url) {
    history.replaceState(null, "", url);
  }
});
document.body.addEventListener("htmx:afterSwap", applyMonbooruBanner);

function applyPauseState() {
  var banner = document.getElementById("queue-paused-banner");
  var btn = document.querySelector("#pause-toggle .pause-btn");
  if (banner && btn) {
    banner.hidden = !btn.classList.contains("paused");
  }
}
document.addEventListener("DOMContentLoaded", applyPauseState);
document.body.addEventListener("htmx:afterSwap", applyPauseState);

// htmx swaps no error status, so a refused action would say nothing. A poll
// fails on every tick of an outage, and base.js has the session's refusals.
document.body.addEventListener("htmx:responseError", function (e) {
  var xhr = e.detail.xhr;
  var trigger = e.detail.elt && e.detail.elt.getAttribute && e.detail.elt.getAttribute("hx-trigger");
  if (!xhr || xhr.status === 401 || xhr.status === 403 || (trigger && trigger.indexOf("every") !== -1)) {
    return;
  }
  var el = showPageNotice(xhr.responseText.trim() || "The server refused that (" + xhr.status + ").");
  if (el) {
    el.dataset.session = "1";
  }
});

// Bound once on the body: htmx swaps the targets in and out.
function onClick(selector, fn) {
  document.body.addEventListener("click", function (e) {
    var el = e.target.closest && e.target.closest(selector);
    if (el) {
      fn(el, e);
    }
  });
}

onClick(".edit-site", function (btn) {
  htmx.ajax("GET", "/settings/sites/" + encodeURIComponent(btn.dataset.site) + "/dialog", "#site-dialog-body");
});

// A refused save renders the dialog with the page; reopen it on what was typed.
document.addEventListener("DOMContentLoaded", function () {
  var body = document.getElementById("site-dialog-body");
  if (body && body.firstElementChild) {
    document.getElementById("site-edit-dialog").showModal();
    applyAuthFields(document.getElementById("sd-auth").value);
    var tab = body.querySelector('.dialog-tab[data-panel="' + body.firstElementChild.dataset.refusedTab + '"]');
    if (tab) {
      tab.click();
    }
  }
});

// ?site=X&tab=mapping&q=tag opens X's dialog on that panel, scrolled to the
// tag rules, with the rule naming q focused or a new row filled for it.
document.addEventListener("DOMContentLoaded", function () {
  var params = new URLSearchParams(location.search);
  var site = params.get("site");
  if (location.pathname !== "/settings" || !site) {
    return;
  }
  var tab = params.get("tab");
  var q = params.get("q");
  document.body.addEventListener("htmx:afterSwap", function onSwap(e) {
    if (e.target.id !== "site-dialog-body") {
      return;
    }
    document.body.removeEventListener("htmx:afterSwap", onSwap);
    var btn = tab && e.target.querySelector('.dialog-tab[data-panel="sd-' + tab + '"]');
    if (btn) {
      btn.click();
    }
    var rules = tab === "mapping" && q && document.getElementById("sd-rules");
    if (!rules) {
      return;
    }
    var from = Array.prototype.find.call(rules.querySelectorAll('input[name="rule_from"]'), function (i) {
      return i.value === q;
    });
    if (!from) {
      rules.appendChild(rules.querySelector("template").content.cloneNode(true));
      from = rules.lastElementChild.querySelector('input[name="rule_from"]');
      from.value = q;
    }
    rules.scrollIntoView({ block: "center" });
    from.closest(".maprow").querySelector('input[name="rule_to"]').focus();
  });
  htmx.ajax("GET", "/settings/sites/" + encodeURIComponent(site) + "/dialog", "#site-dialog-body");
});

// A refusal re-renders without a #section anchor, leaving its flash below
// the fold; a redirect brings its own.
document.addEventListener("DOMContentLoaded", function () {
  if (location.hash) {
    return;
  }
  var flash = document.querySelector("#content .flash");
  if (flash) {
    flash.scrollIntoView({ block: "center" });
  }
});


onClick(".item-debug", function (btn) {
  htmx.ajax("GET", "/internal/item-debug?url=" + encodeURIComponent(btn.dataset.url), "#debug-log-body");
});

function applyAuthFields(kind) {
  var family = document.getElementById("sd-family").value;
  var show = {
    "auth-none": kind === "none",
    "auth-oauth": kind === "oauth",
    "auth-username": kind === "api_optional" || kind === "username_password",
    "auth-password": kind === "username_password",
    "auth-apikey": kind === "api_optional" || kind === "api_required",
    "auth-userid": kind === "api_required" || family === "danbooru",
    "auth-cookies": kind === "cookies",
  };
  Object.keys(show).forEach(function (cls) {
    document.querySelectorAll("#site-dialog-body ." + cls).forEach(function (el) {
      el.style.display = show[cls] ? "" : "none";
    });
  });
}
document.body.addEventListener("change", function (e) {
  if (e.target.id === "sd-auth" || e.target.id === "sd-family") {
    applyAuthFields(document.getElementById("sd-auth").value);
  }
});
// htmx.ajax resolves on a failed fetch too, so a dialog opens only once its body
// swapped, or a failure would show the last site's form under a Save for it.
function openOnSwap(target) {
  var dialog = target.closest("dialog");
  if (dialog && !dialog.open) {
    dialog.showModal();
  }
}
document.body.addEventListener("htmx:afterSwap", function (e) {
  if (e.target.id === "site-dialog-body") {
    openOnSwap(e.target);
    var auth = document.getElementById("sd-auth");
    if (auth) {
      applyAuthFields(auth.value);
    }
  } else if (e.target.id === "debug-log-body") {
    openOnSwap(e.target);
  }
});

onClick(".add-maprow", function (add) {
  var rows = document.getElementById(add.dataset.rows);
  rows.appendChild(rows.querySelector("template").content.cloneNode(true));
});
onClick(".maprow-del", function (del) {
  del.closest(".maprow").remove();
});


var chainDragging = null;
document.body.addEventListener("dragstart", function (e) {
  var li = e.target.closest && e.target.closest("#chain-dialog li");
  if (!li) {
    return;
  }
  chainDragging = li;
  e.dataTransfer.effectAllowed = "move";
  // Firefox refuses to start a drag with no payload attached.
  e.dataTransfer.setData("text/plain", li.dataset.name);
});
document.body.addEventListener("dragover", function (e) {
  if (!chainDragging) {
    return;
  }
  var list = e.target.closest && e.target.closest("#chain-dialog ul");
  if (!list) {
    return;
  }
  e.preventDefault();
  var before = null;
  list.querySelectorAll("li").forEach(function (li) {
    if (li !== chainDragging && !before && e.clientY < li.getBoundingClientRect().top + li.offsetHeight / 2) {
      before = li;
    }
  });
  if (before !== chainDragging.nextSibling || chainDragging.parentNode !== list) {
    list.insertBefore(chainDragging, before);
  }
});
document.body.addEventListener("dragend", function () {
  chainDragging = null;
});

// HTML5 drag works from neither keyboard nor touch, so each row carries
// its moves as buttons too.
onClick(".chain-move", function (btn) {
  var li = btn.closest("li");
  var move = btn.dataset.move;
  if (move === "up") {
    if (li.previousElementSibling) {
      li.parentNode.insertBefore(li, li.previousElementSibling);
    }
  } else if (move === "down") {
    if (li.nextElementSibling) {
      li.parentNode.insertBefore(li.nextElementSibling, li);
    }
  } else {
    document.getElementById(move).appendChild(li);
    li.querySelector(move === "chain-on" ? '[data-move="up"]' : '[data-move="chain-on"]').focus();
    return;
  }
  btn.focus();
});
document.addEventListener("DOMContentLoaded", function () {
  var form = document.getElementById("chain-form");
  if (!form) {
    return;
  }
  // The lists are reordered in place, so closing restores them, or an
  // abandoned drag would ride the next save.
  var lists = ["chain-on", "chain-off"].map(function (id) { return document.getElementById(id); });
  var saved = lists.map(function (list) { return Array.prototype.slice.call(list.children); });
  document.getElementById("chain-dialog").addEventListener("close", function () {
    lists.forEach(function (list, i) {
      saved[i].forEach(function (li) { list.appendChild(li); });
    });
  });
  form.addEventListener("submit", function () {
    form.querySelectorAll("input[name^='order-']").forEach(function (i) { i.remove(); });
    var add = function (name, value) {
      var input = document.createElement("input");
      input.type = "hidden";
      input.name = "order-" + name;
      input.value = value;
      form.appendChild(input);
    };
    document.querySelectorAll("#chain-on li").forEach(function (li, i) { add(li.dataset.name, i + 1); });
    document.querySelectorAll("#chain-off li").forEach(function (li) { add(li.dataset.name, ""); });
  });
});

// The chords come from the data-chord links; a key typed into a field is the
// field's.
document.addEventListener("keydown", function (e) {
  var tag = e.target.tagName.toLowerCase();
  var isInput = tag === "input" || tag === "textarea" || tag === "select" || e.target.isContentEditable;
  if (e.key === "Escape") {
    if (isInput) {
      e.target.blur();
    }
    return;
  }
  if (isInput || e.ctrlKey || e.metaKey || e.altKey) {
    return;
  }
  var help = document.getElementById("shortcuts-help");
  if (document.querySelector("dialog[open]")) {
    if (e.key === "?" && help && help.open) {
      e.preventDefault();
      help.close();
    }
    return;
  }
  if (chordStep(e)) {
    return;
  }
  if (e.key === "?" && help) {
    e.preventDefault();
    openShortcutSheet();
    return;
  }
  if (e.key === "s" || e.key === "/") {
    var bar = document.querySelector(".add-bar input[name=url]");
    if (bar && !bar.disabled) {
      e.preventDefault();
      bar.focus();
    }
    return;
  }
  var chords = chordMapFromPage();
  if (chords[e.key]) {
    e.preventDefault();
    enterChord(chords[e.key], e.key);
  }
});
