document.addEventListener("DOMContentLoaded", function () {
  var bar = document.getElementById("add-input");
  if (bar) {
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
      } else {
        expandedMore.delete(owner.dataset.job);
      }
    }
  }
}, true);
document.addEventListener("DOMContentLoaded", applyJobStates);
document.body.addEventListener("htmx:afterSwap", function (e) {
  if (e.target && e.target.id === "queue-rows") {
    applyJobStates();
  }
});

// A state outside this list, "checking" included, keeps the server-rendered
// banner, so it does not flash.
function applyMonbooruState(conn) {
  if (conn !== "down" && conn !== "ok" && conn !== "rejected" && conn !== "unpaired" &&
      conn !== "paused" && conn !== "unconfigured") {
    return;
  }
  var blocked = conn !== "ok";
  var banner = document.getElementById("monbooru-banner");
  if (banner) {
    banner.hidden = !blocked;
    var msg = document.getElementById("monbooru-banner-msg");
    if (msg && blocked) {
      msg.textContent = conn === "rejected" ? "monbooru rejected the api token"
        : conn === "unpaired" ? "monbooru is not paired"
        : conn === "unconfigured" ? "monbooru is not configured"
        : conn === "paused" ? "the monbooru link is paused - downloads are disabled until you resume it from the footer light"
        : "monbooru is unreachable";
    }
    // A pause is the operator's own doing, so the banner points at the
    // light, not the settings.
    var fix = document.getElementById("monbooru-banner-fix");
    if (fix) {
      fix.hidden = conn === "paused";
    }
  }
  document.querySelectorAll(".needs-monbooru").forEach(function (el) {
    el.disabled = blocked;
  });
}
document.body.addEventListener("htmx:afterSwap", function () {
  var light = document.getElementById("conn-light");
  if (light) {
    applyMonbooruState(light.dataset.conn);
  }
});

function applyPauseState() {
  var banner = document.getElementById("queue-paused-banner");
  var btn = document.querySelector("#pause-toggle .pause-btn");
  if (banner && btn) {
    banner.hidden = !btn.classList.contains("paused");
  }
}
document.addEventListener("DOMContentLoaded", applyPauseState);
document.body.addEventListener("htmx:afterSwap", applyPauseState);

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
  htmx.ajax("GET", "/settings/sites/" + encodeURIComponent(btn.dataset.site) + "/dialog", "#site-dialog-body").then(function () {
    document.getElementById("site-edit-dialog").showModal();
  });
});

// A refused save renders the dialog with the page; reopen it on what was typed.
document.addEventListener("DOMContentLoaded", function () {
  var body = document.getElementById("site-dialog-body");
  if (body && body.firstElementChild) {
    document.getElementById("site-edit-dialog").showModal();
    applyAuthFields(document.getElementById("sd-auth").value);
  }
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

// Hidden panels still submit, so one save writes every tab.
onClick(".dialog-tab", function (tab) {
  var dlg = tab.closest("dialog");
  dlg.querySelectorAll(".dialog-tab").forEach(function (t) { t.classList.toggle("active", t === tab); });
  dlg.querySelectorAll(".dialog-panel").forEach(function (p) { p.hidden = p.id !== tab.dataset.panel; });
});

// execCommand covers origins without the clipboard API: a plain-http LAN
// page is not a secure context.
onClick("[data-copy-from]", function (btn, e) {
  e.preventDefault();
  var src = document.querySelector(btn.dataset.copyFrom);
  var text = src ? (src.value !== undefined ? src.value : src.textContent) : "";
  var flash = btn.parentElement && btn.parentElement.querySelector(".copy-flash");
  var done = function () {
    if (!flash) { return; }
    flash.hidden = false;
    setTimeout(function () { flash.hidden = true; }, 1500);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text, done); });
  } else {
    fallbackCopy(text, done);
  }
});

function fallbackCopy(text, done) {
  var ta = document.createElement("textarea");
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); } catch (err) { /* fall through */ }
  document.body.removeChild(ta);
  done();
}

onClick(".item-debug", function (btn) {
  htmx.ajax("GET", "/internal/item-debug?url=" + encodeURIComponent(btn.dataset.url), "#debug-log-body").then(function () {
    document.getElementById("item-debug-dialog").showModal();
  });
});

function applyAuthFields(kind) {
  var show = {
    "auth-none": kind === "none",
    "auth-oauth": kind === "oauth",
    "auth-username": kind === "api_optional" || kind === "username_password",
    "auth-password": kind === "username_password",
    "auth-apikey": kind === "api_optional" || kind === "api_required",
    "auth-userid": kind === "api_required",
    "auth-cookies": kind === "cookies",
  };
  Object.keys(show).forEach(function (cls) {
    document.querySelectorAll("#site-dialog-body ." + cls).forEach(function (el) {
      el.style.display = show[cls] ? "" : "none";
    });
  });
}
document.body.addEventListener("change", function (e) {
  if (e.target.id === "sd-auth") {
    applyAuthFields(e.target.value);
  }
});
document.body.addEventListener("htmx:afterSwap", function (e) {
  if (e.target.id === "site-dialog-body") {
    var auth = document.getElementById("sd-auth");
    if (auth) {
      applyAuthFields(auth.value);
    }
  }
});

onClick(".add-maprow", function (add) {
  var rows = document.getElementById(add.dataset.rows);
  rows.appendChild(rows.querySelector("template").content.cloneNode(true));
});
onClick(".maprow-del", function (del) {
  del.closest(".maprow").remove();
});

// Browse only opens the dialog: the button's own hx-get fetches the listing.
document.addEventListener("click", function (e) {
  if (e.target.closest(".dir-picker-open")) {
    var dlg = document.getElementById("dir-picker-dialog");
    if (dlg && !dlg.open) dlg.showModal();
    return;
  }
  var use = e.target.closest(".dir-picker-use");
  if (!use) return;
  var input = document.getElementById(use.dataset.into || "");
  if (input) {
    input.value = use.dataset.path || "";
    input.dispatchEvent(new Event("input", { bubbles: true }));
  }
  var open = use.closest("dialog");
  if (open) open.close();
});

// The polls' trigger filters read this: a swap under the open pop-in
// detaches the button whose request is pending, and htmx drops a request
// from a detached element.
function confirmOpen() {
  var dlg = document.getElementById("confirm-dialog");
  return !!(dlg && dlg.open);
}
function showConfirm(message, onOk, okLabel, danger, detail) {
  var dlg = document.getElementById("confirm-dialog");
  if (!dlg) {
    if (window.confirm(detail ? message + "\n\n" + detail : message)) {
      onOk();
    }
    return;
  }
  document.getElementById("confirm-dialog-msg").textContent = message || "";
  var detailEl = document.getElementById("confirm-dialog-detail");
  if (detailEl) {
    detailEl.textContent = detail || "";
    detailEl.style.display = detail ? "" : "none";
  }
  var okBtn = document.getElementById("confirm-dialog-ok");
  var cancelBtn = document.getElementById("confirm-dialog-cancel");
  okBtn.textContent = okLabel || "ok";
  okBtn.classList.toggle("btn-danger", !!danger);
  var close = function () {
    dlg.close();
    okBtn.onclick = null;
    cancelBtn.onclick = null;
  };
  okBtn.onclick = function () {
    close();
    onOk();
  };
  cancelBtn.onclick = close;
  dlg.showModal();
  (danger ? cancelBtn : okBtn).focus();
}
document.body.addEventListener("htmx:confirm", function (e) {
  if (!e.detail || !e.detail.question) {
    return;
  }
  e.preventDefault();
  var elt = e.detail.elt;
  var okLabel = elt && elt.dataset ? elt.dataset.confirmOk : "";
  var danger = !!(elt && elt.hasAttribute && elt.hasAttribute("data-confirm-danger"));
  // The attribute doubles as the warning subtext when it carries a value.
  var detail = elt && elt.getAttribute ? elt.getAttribute("data-confirm-danger") : "";
  showConfirm(e.detail.question, function () { e.detail.issueRequest(true); }, okLabel, danger, detail);
});

// htmx:confirm only fires for htmx requests, so hx-confirm on a plain form is
// handled here; form.submit() fires no submit event, so it cannot loop.
document.body.addEventListener("submit", function (e) {
  var form = e.target;
  if (!form || !form.hasAttribute || !form.hasAttribute("hx-confirm")) {
    return;
  }
  if (form.hasAttribute("hx-get") || form.hasAttribute("hx-post") ||
      form.hasAttribute("hx-put") || form.hasAttribute("hx-patch") || form.hasAttribute("hx-delete")) {
    return;
  }
  e.preventDefault();
  showConfirm(form.getAttribute("hx-confirm"), function () { form.submit(); },
    form.dataset.confirmOk, form.hasAttribute("data-confirm-danger"),
    form.getAttribute("data-confirm-danger"));
});

document.body.addEventListener("token-saved", function (e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
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
