// Focus the command bar on the add screen so a pasted URL needs no click.
document.addEventListener("DOMContentLoaded", function () {
  var bar = document.getElementById("add-input");
  if (bar) {
    bar.focus();
  }
});

// A job's items render expanded. The 2s poll and an in-place add swap the whole
// rows body, re-emitting each job open and its active "+N more" sub-list closed,
// so remember which jobs the operator collapsed and which "+N more" lists they
// expanded (toggle does not bubble, so capture it) and re-apply both after a
// swap. The collapsed set rides in sessionStorage so a full page refresh, which
// reloads the rows from scratch, keeps the operator's collapsed rows collapsed.
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

// Reflect monbooru connectivity into the add forms. The footer light polls
// /internal/monbooru-status and swaps its live state onto data-conn; when
// monbooru is unreachable, unpaired, rejecting the token, or the operator has
// paused the link from the light, reveal the top banner and block URL
// submission (a queued download could only fail at the push step). The
// transient "checking" state leaves the server-rendered state in place so the
// banner does not flash.
function applyMonbooruState(conn) {
  if (conn !== "down" && conn !== "ok" && conn !== "rejected" && conn !== "unpaired" && conn !== "paused") {
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
        : conn === "paused" ? "the monbooru link is paused - downloads are disabled until you resume it from the footer light"
        : "monbooru is unreachable";
    }
    // A paused link is the operator's own doing, so it points back at the
    // light rather than at the connection settings.
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

// Delegated clicks: the queue rows, the site dialog and the mapping editors are
// all swapped in by htmx, so every handler binds once on the body and matches
// the closest element instead of re-binding after each swap.
function onClick(selector, fn) {
  document.body.addEventListener("click", function (e) {
    var el = e.target.closest && e.target.closest(selector);
    if (el) {
      fn(el, e);
    }
  });
}

// Per-site settings edit: the dialog's whole body is one server-rendered
// fragment (credentials as set/unset placeholders, the effective profile),
// loaded fresh each time it opens so a row, a search hit, and a lookup
// shortcut all share one entry point that needs only the site name.
onClick(".edit-site", function (btn) {
  htmx.ajax("GET", "/settings/sites/" + encodeURIComponent(btn.dataset.site) + "/dialog", "#site-dialog-body").then(function () {
    document.getElementById("site-edit-dialog").showModal();
  });
});

// The dialog's tab strip switches its panels; hidden panels' fields still
// submit, so the one save button writes every tab.
onClick(".dialog-tab", function (tab) {
  var dlg = tab.closest("dialog");
  dlg.querySelectorAll(".dialog-tab").forEach(function (t) { t.classList.toggle("active", t === tab); });
  dlg.querySelectorAll(".dialog-panel").forEach(function (p) { p.hidden = p.id !== tab.dataset.panel; });
});

// The export tab's copy button reads the profile file from its hidden
// textarea; the execCommand fallback covers contexts without the
// clipboard API (plain-http LAN origins).
onClick("[data-copy-from]", function (btn, e) {
  e.preventDefault();
  var src = document.querySelector(btn.dataset.copyFrom);
  var text = src ? src.value : "";
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

// The auth tab shows only the credentials the selected login type uses;
// changing the type retargets the fields at once. Without JS every field
// stays visible, which still saves correctly.
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

// The mapping tab's editors grow from their row template and shrink with
// each row's remove button.
onClick(".add-maprow", function (add) {
  var rows = document.getElementById(add.dataset.rows);
  rows.appendChild(rows.querySelector("template").content.cloneNode(true));
});
onClick(".maprow-del", function (del) {
  del.closest(".maprow").remove();
});

// Route hx-confirm prompts through an in-page pop-in instead of the browser's
// native confirm(). A destructive action carries data-confirm-danger, which
// reddens the OK button and lands focus on cancel so an accidental Enter does
// not commit it.
// The queue poll's trigger filter reads this: swapping #queue-rows while the
// pop-in is open would detach the button whose confirmed request is pending,
// and htmx silently drops a request from a detached element.
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

// htmx raises htmx:confirm only for requests it issues itself, so an hx-confirm
// on a plain form (the ptr delete) would be inert; intercept the native submit
// and route it through the same pop-in. form.submit() re-fires no submit event,
// so a confirmed form posts without looping back here.
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

// The per-token privileges dialog closes itself on a successful save; the
// scopes cell and the parent flash arrive as OOB swaps.
document.body.addEventListener("token-saved", function (e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
});

// Lookup-chain dialog: drag a source between or within the two columns; on
// save the queried column's top-to-bottom order becomes the chain order, so
// the form serializes the lists into the order-<name> fields the handler
// already reads.
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
document.addEventListener("DOMContentLoaded", function () {
  var form = document.getElementById("chain-form");
  if (!form) {
    return;
  }
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
