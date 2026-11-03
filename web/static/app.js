// app.js — progressive enhancement for the admin UI. No external dependencies.
// Every feature here is an enhancement: each page works without JavaScript.
//
// Dropzone: a drag-and-drop upload surface with a staged file list. Dropping or
// browsing ACCUMULATES files (it does not replace the previous selection), each
// staged file gets a remove button, and the real <input type="file"> is kept in
// sync so a normal form submit uploads exactly the staged set. Changes are
// announced through the zone's [data-dz-status] live region.
//
// Autofill: on the new-project form, staging a single HTML file offers a
// button that copies its name into Name and Slug.
//
// Slug: [data-slug-target] follows [data-slug-source] (slugified) until the
// user edits the slug; [data-slug-preview] shows the resulting URL.
//
// Busy states: a submitting form's button gets .is-busy (spinner, optional
// [data-busy-label] text) and <html> gets .is-busy (header request bar), for
// native submits and htmx requests alike. Double submits are ignored.
//
// Upload progress: htmx uploads fill the form's [data-upload-progress].
//
// Announcements: after an htmx swap, a success flash ([data-flash]) is copied
// into the layout's persistent #pp-announcer live region.
//
// Flash cleanup: ?flash= / ?error= are dropped from the address bar after the
// page shows them, so a reload does not repeat a stale message.
//
// File filter: [data-file-filter] (revealed for 8+ files) filters rows by path.
//
// Editor: [data-editor] tracks unsaved changes (status text, beforeunload and
// an htmx:confirm guard), and Ctrl/Cmd+S saves.
//
// Copy buttons: a [data-copy="<id>"] button copies the value (input/textarea)
// or text (pre/code) of the element with that id. Buttons ship `hidden` and
// are revealed here, so without JavaScript the readonly field (select on focus)
// is the copy path. See initCopy for the fallback chain.
//
// Init runs on DOMContentLoaded and after every htmx swap, guarded so each
// element is wired exactly once.
(function () {
  "use strict";

  var isMac = /Mac|iPhone|iPad/.test(navigator.platform || "");
  var reducedMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)");

  function motionOK() {
    return !(reducedMotion && reducedMotion.matches);
  }

  function slugify(s) {
    return String(s)
      .normalize("NFKD")
      .replace(/[̀-ͯ]/g, "")
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 63)
      .replace(/-+$/, "");
  }

  function baseName(path) {
    return String(path).split(/[\\/]/).pop() || "";
  }

  function stripExt(name) {
    return name.replace(/\.[^.]+$/, "");
  }

  function isHTML(name) {
    return /\.html?$/i.test(name);
  }

  function humanBytes(n) {
    if (n < 1024) return n + " B";
    var units = ["KB", "MB", "GB", "TB"], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + " " + units[i];
  }

  function plural(n, one, many) {
    return n + " " + (n === 1 ? one : many);
  }

  // say writes to a live region. Clearing first makes a repeated identical
  // message be read again.
  function say(region, msg) {
    if (!region) return;
    region.textContent = "";
    setTimeout(function () { region.textContent = msg; }, 60);
  }

  // ---------- Busy states ----------

  var inFlight = 0;

  function setPageBusy(delta) {
    inFlight = Math.max(0, inFlight + delta);
    document.documentElement.classList.toggle("is-busy", inFlight > 0);
  }

  function markBusy(btn, label) {
    if (!btn || btn.classList.contains("is-busy")) return;
    btn.classList.add("is-busy");
    btn.setAttribute("aria-disabled", "true");
    // Swap the label only on plain-text buttons (no sr-only spans to keep).
    if (label && !btn.children.length) {
      btn.dataset.idleLabel = btn.textContent;
      btn.textContent = label;
    }
  }

  function clearBusy(btn) {
    if (!btn) return;
    btn.classList.remove("is-busy");
    btn.removeAttribute("aria-disabled");
    if (btn.dataset.idleLabel) {
      btn.textContent = btn.dataset.idleLabel;
      delete btn.dataset.idleLabel;
    }
  }

  function submitterOf(form, e) {
    return (e && e.submitter) ||
      form.querySelector('button[type="submit"], button:not([type])') ||
      (form.id && document.querySelector('button[type="submit"][form="' + form.id + '"]'));
  }

  // Capture phase: remember which button submitted (htmx forms read it later).
  document.addEventListener("submit", function (e) {
    var form = e.target;
    form._ppSubmitter = e.submitter || null;
  }, true);

  // Bubble phase: native (non-htmx) submits. htmx and cancelled confirm()s
  // have already called preventDefault, so they are skipped here.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (e.defaultPrevented) return;
    if (form.dataset.submitting === "1") { e.preventDefault(); return; }
    form.dataset.submitting = "1";
    markBusy(submitterOf(form, e), form.getAttribute("data-busy-label"));
    setPageBusy(1);
  });

  // Back/forward cache restores the page as it was when we left: reset.
  window.addEventListener("pageshow", function (e) {
    if (!e.persisted) return;
    inFlight = 0;
    setPageBusy(0);
    document.querySelectorAll("form[data-submitting]").forEach(function (f) { delete f.dataset.submitting; });
    document.querySelectorAll(".btn.is-busy").forEach(clearBusy);
  });

  function busyTargetFor(elt) {
    if (!elt) return null;
    if (elt.tagName === "FORM") return submitterOf(elt, { submitter: elt._ppSubmitter });
    return elt.classList && elt.classList.contains("btn") ? elt : null;
  }

  document.addEventListener("htmx:beforeRequest", function (e) {
    var elt = e.detail && e.detail.elt;
    setPageBusy(1);
    var btn = busyTargetFor(elt);
    markBusy(btn, elt && elt.getAttribute && elt.getAttribute("data-busy-label"));
    if (elt) elt._ppBusyBtn = btn;
  });

  document.addEventListener("htmx:afterRequest", function (e) {
    var elt = e.detail && e.detail.elt;
    setPageBusy(-1);
    if (elt && elt._ppBusyBtn) { clearBusy(elt._ppBusyBtn); elt._ppBusyBtn = null; }
    if (elt) hideProgress(elt);
  });

  // ---------- Upload progress (htmx uploads) ----------

  function progressParts(form) {
    var box = form.querySelector && form.querySelector("[data-upload-progress]");
    if (!box) return null;
    return { box: box, bar: box.querySelector("progress"), text: box.querySelector("[data-upload-progress-text]") };
  }

  function hideProgress(form) {
    var p = progressParts(form);
    if (!p) return;
    p.box.hidden = true;
    p.bar.value = 0;
    form._ppMaxPct = 0;
  }

  document.addEventListener("htmx:xhr:progress", function (e) {
    var form = e.target;
    var d = e.detail || {};
    var p = progressParts(form);
    if (!p || !d.lengthComputable || !d.total) return;
    // Only worth showing for uploads that take a moment (> ~200 KB).
    if (d.total < 200000 && p.box.hidden) return;
    // Upload and response download fire the same event; never go backwards.
    var pct = Math.min(100, Math.round((d.loaded / d.total) * 100));
    if (pct < (form._ppMaxPct || 0)) return;
    form._ppMaxPct = pct;
    p.box.hidden = false;
    p.bar.value = pct;
    p.text.textContent = pct < 100 ? "Uploading… " + pct + "%" : "Processing…";
  });

  // ---------- Announce flashes after htmx swaps ----------

  document.addEventListener("htmx:afterSettle", function () {
    var main = document.getElementById("app-main");
    var flash = main && main.querySelector("[data-flash]");
    if (flash) say(document.getElementById("pp-announcer"), flash.textContent.trim());
  });

  // ---------- Drop stale ?flash= / ?error= from the address bar ----------

  function cleanFlashParams() {
    if (!window.history || !history.replaceState || !window.URLSearchParams) return;
    var url = new URL(window.location.href);
    if (!url.searchParams.has("flash") && !url.searchParams.has("error")) return;
    url.searchParams.delete("flash");
    url.searchParams.delete("error");
    history.replaceState(history.state, "", url.pathname + (url.searchParams.toString() ? "?" + url.searchParams.toString() : "") + url.hash);
  }

  // ---------- Dropzone ----------

  // installGlobalDropGuard stops the browser from navigating to a file dropped
  // anywhere on the page (including just outside a dropzone). Installed once.
  var dropGuardInstalled = false;
  function installGlobalDropGuard() {
    if (dropGuardInstalled) return;
    dropGuardInstalled = true;
    window.addEventListener("dragover", function (e) { e.preventDefault(); });
    window.addEventListener("drop", function (e) { e.preventDefault(); });
  }

  // initDropzone wires one [data-dropzone]. It keeps a DataTransfer as the
  // authoritative staged set, merges new files into it (deduped), mirrors it onto
  // the input, and renders a removable list. It emits a "dz:change" event on the
  // zone after any change so autofill can react.
  function initDropzone(zone) {
    var input = zone.querySelector('input[type="file"]');
    if (!input) return;
    var listEl = zone.querySelector("[data-dz-list]");
    var status = zone.querySelector("[data-dz-status]");
    var staged = new DataTransfer();
    var syncing = false;
    var fresh = {}; // lower-cased names added by the last change (animate in)

    function emitChange() {
      zone.dispatchEvent(new CustomEvent("dz:change"));
    }

    function mirror(focusIdx) {
      // Assigning input.files does not fire "change"; the guard is belt-and-braces.
      syncing = true;
      input.files = staged.files;
      syncing = false;
      renderList(focusIdx);
      zone.classList.toggle("dropzone--filled", staged.files.length > 0);
      emitChange();
    }

    function addFiles(fileList) {
      if (!fileList || !fileList.length) return;
      // Merge by base name (case-insensitive), preserving order. A newer file
      // with an existing name replaces the staged one — the server stores by
      // base name, so one entry per filename keeps staging and result in step.
      var byName = {}, order = [];
      fresh = {};
      function put(f, isNew) {
        var k = f.name.toLowerCase();
        if (!(k in byName)) order.push(k);
        byName[k] = f;
        if (isNew) fresh[k] = true;
      }
      for (var i = 0; i < staged.files.length; i++) put(staged.files[i], false);
      for (var j = 0; j < fileList.length; j++) put(fileList[j], true);
      staged = new DataTransfer();
      order.forEach(function (k) { staged.items.add(byName[k]); });
      mirror();
      var n = staged.files.length;
      say(status, plural(fileList.length, "file", "files") + " added. " +
        plural(n, "file", "files") + " ready to upload.");
    }

    function removeAt(idx) {
      var removed = staged.files[idx];
      var kept = [];
      for (var i = 0; i < staged.files.length; i++) if (i !== idx) kept.push(staged.files[i]);
      staged = new DataTransfer();
      kept.forEach(function (f) { staged.items.add(f); });
      fresh = {};
      mirror(idx);
      say(status, "Removed " + removed.name + ". " +
        (kept.length ? plural(kept.length, "file", "files") + " ready to upload." : "No files selected."));
    }

    function clearAll() {
      staged = new DataTransfer();
      fresh = {};
      mirror();
      input.focus();
      say(status, "Selection cleared.");
    }

    // Focus continuity after a removal: the next file's remove button, else
    // the previous one, else the file input itself.
    function renderList(focusIdx) {
      if (!listEl) return;
      listEl.textContent = "";
      var n = staged.files.length;
      listEl.hidden = n === 0;
      if (n === 0) {
        if (focusIdx !== undefined) input.focus();
        return;
      }

      var head = document.createElement("div");
      head.className = "dz-files__head";
      var count = document.createElement("span");
      var total = 0;
      for (var t = 0; t < n; t++) total += staged.files[t].size;
      count.textContent = plural(n, "file", "files") + " selected · " + humanBytes(total);
      var clear = document.createElement("button");
      clear.type = "button";
      clear.className = "dz-files__clear";
      clear.textContent = "Clear all";
      clear.addEventListener("click", clearAll);
      head.appendChild(count);
      head.appendChild(clear);
      listEl.appendChild(head);

      var ul = document.createElement("ul");
      ul.className = "dz-files__list";
      var buttons = [];
      for (var i = 0; i < n; i++) {
        (function (idx, f) {
          var li = document.createElement("li");
          li.className = "dz-file" + (fresh[f.name.toLowerCase()] ? " dz-file--new" : "");
          var name = document.createElement("span");
          name.className = "dz-file__name";
          name.textContent = f.name;
          name.title = f.name;
          var size = document.createElement("span");
          size.className = "dz-file__size";
          size.textContent = humanBytes(f.size);
          var rm = document.createElement("button");
          rm.type = "button"; // never submit the form
          rm.className = "dz-file__remove";
          rm.setAttribute("aria-label", "Remove " + f.name);
          rm.textContent = "×";
          rm.addEventListener("click", function () {
            if (!motionOK()) { removeAt(idx); return; }
            li.classList.add("dz-file--leaving");
            setTimeout(function () { removeAt(idx); }, 180);
          });
          buttons.push(rm);
          li.appendChild(name);
          li.appendChild(size);
          li.appendChild(rm);
          ul.appendChild(li);
        })(i, staged.files[i]);
      }
      listEl.appendChild(ul);
      if (focusIdx !== undefined) {
        (buttons[Math.min(focusIdx, buttons.length - 1)] || input).focus();
      }
    }

    // Browse (native change) → merge, not replace.
    input.addEventListener("change", function () {
      if (syncing) return;
      addFiles(input.files);
    });

    // Drag highlight + drop-to-stage on the whole zone.
    ["dragenter", "dragover"].forEach(function (ev) {
      zone.addEventListener(ev, function (e) {
        e.preventDefault();
        e.stopPropagation();
        zone.classList.add("dropzone--drag");
      });
    });
    zone.addEventListener("dragleave", function (e) {
      if (zone.contains(e.relatedTarget)) return;
      zone.classList.remove("dropzone--drag");
    });
    zone.addEventListener("dragend", function () {
      zone.classList.remove("dropzone--drag");
    });
    zone.addEventListener("drop", function (e) {
      e.preventDefault();
      e.stopPropagation();
      zone.classList.remove("dropzone--drag");
      if (e.dataTransfer) addFiles(e.dataTransfer.files);
    });

    // A form reset (or a native submit restored from history) empties the input.
    var form = zone.closest("form");
    if (form) form.addEventListener("reset", function () { staged = new DataTransfer(); mirror(); });

    renderList();
  }

  // initAutofill wires the opt-in "use file name" button on the new-project form.
  // The button appears only when exactly one HTML file is staged; clicking it
  // fills Name and Slug from that file name (overwriting whatever is there — it
  // is an explicit user action). Nothing is filled automatically.
  function initAutofill(zone) {
    var input = zone.querySelector('input[type="file"]');
    if (!input) return;
    var form = zone.closest("form");
    if (!form) return;
    var nameEl = form.querySelector('input[name="name"]');
    var slugEl = form.querySelector('input[name="slug"]');
    var btn = form.querySelector("[data-autofill-btn]");
    if (!btn || (!nameEl && !slugEl)) return;

    var pending = null; // file-name stem to apply, or null when not applicable

    function stagedHTMLStem() {
      if (input.files && input.files.length === 1) {
        var nm = baseName(input.files[0].name);
        if (isHTML(nm)) return stripExt(nm);
      }
      return null;
    }

    function refresh() {
      pending = stagedHTMLStem();
      if (pending) {
        btn.hidden = false;
        btn.textContent = "Use “" + pending + "” for Name & Slug";
      } else {
        btn.hidden = true;
      }
    }

    btn.addEventListener("click", function () {
      if (!pending) return;
      if (nameEl) nameEl.value = pending;
      if (slugEl) {
        slugEl.value = slugify(pending);
        slugEl.dispatchEvent(new Event("input", { bubbles: true }));
      }
      (nameEl || slugEl).focus();
    });

    zone.addEventListener("dz:change", refresh);
    refresh();
  }

  // ---------- Slug follows name + URL preview ----------

  function initSlug(target) {
    var form = target.closest("form");
    var source = form && form.querySelector("[data-slug-source]");
    var preview = form && form.querySelector("[data-slug-preview]");
    var valid = /^[a-z0-9][a-z0-9-]*$/;
    // A slug that is already filled (re-render after an error) counts as the
    // user's own; an empty one follows the name.
    var touched = target.value !== "";

    function renderPreview() {
      if (!preview) return;
      var v = target.value;
      if (!v || !valid.test(v)) { preview.hidden = true; return; }
      preview.textContent = "";
      preview.appendChild(document.createTextNode("Will be served at "));
      var code = document.createElement("code");
      code.textContent = window.location.origin + "/" + v + "/";
      preview.appendChild(code);
      preview.hidden = false;
    }

    if (source) {
      source.addEventListener("input", function () {
        if (touched) return;
        target.value = slugify(source.value);
        renderPreview();
      });
    }
    target.addEventListener("input", function (e) {
      // Programmatic follow updates do not dispatch input; real typing does.
      if (e.isTrusted) touched = target.value !== "";
      renderPreview();
    });
    // Tidy what was typed ("My Site" -> "my-site") when leaving the field.
    target.addEventListener("change", function () {
      if (target.value && !valid.test(target.value)) {
        target.value = slugify(target.value);
        renderPreview();
      }
    });
    renderPreview();
  }

  // ---------- File filter ----------

  var lastFilter = { path: "", value: "" };

  function initFileFilter(box) {
    var card = box.closest(".card");
    var input = box.querySelector("input");
    var countEl = box.querySelector("[data-file-filter-count]");
    var emptyEl = card && card.querySelector("[data-file-filter-empty]");
    var rows = card ? Array.prototype.slice.call(card.querySelectorAll("tbody tr[data-path]")) : [];
    if (!input || rows.length < 8) return;
    box.hidden = false;
    var timer = null;

    function apply(announce) {
      var q = input.value.trim().toLowerCase();
      var shown = 0;
      rows.forEach(function (tr) {
        var hit = !q || tr.getAttribute("data-path").toLowerCase().indexOf(q) !== -1;
        tr.hidden = !hit;
        if (hit) shown++;
      });
      if (emptyEl) emptyEl.hidden = shown !== 0;
      lastFilter = { path: window.location.pathname, value: input.value };
      if (!announce) return;
      clearTimeout(timer);
      timer = setTimeout(function () {
        say(countEl, q ? shown + " of " + plural(rows.length, "file", "files") + " shown." : "Showing all files.");
      }, 400);
    }

    input.addEventListener("input", function () { apply(true); });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && input.value) {
        e.preventDefault();
        input.value = "";
        apply(true);
      }
    });
    // Keep the filter across htmx swaps of the same page (delete, rename…).
    if (lastFilter.path === window.location.pathname && lastFilter.value) {
      input.value = lastFilter.value;
      apply(false);
    }
  }

  // ---------- Editor: unsaved changes + Ctrl/Cmd+S ----------

  var dirtyEditor = null; // the form whose textarea has unsaved changes

  function initEditor(form) {
    var ta = form.querySelector("textarea");
    var status = form.querySelector("[data-editor-status]");
    if (!ta) return;
    var initial = ta.value;
    var hint = (isMac ? "⌘S" : "Ctrl+S") + " saves";
    var wasDirty = false;

    function update() {
      var dirty = ta.value !== initial;
      dirtyEditor = dirty ? form : (dirtyEditor === form ? null : dirtyEditor);
      if (!status || dirty === wasDirty) return;
      wasDirty = dirty;
      status.classList.toggle("is-dirty", dirty);
      status.textContent = dirty ? "Unsaved changes · " + hint : hint;
    }

    if (status) status.textContent = hint;
    ta.addEventListener("input", update);
    form.addEventListener("keydown", function (e) {
      if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === "s" || e.key === "S")) {
        e.preventDefault();
        if (form.requestSubmit) form.requestSubmit(); else form.submit();
      }
    });
    form.addEventListener("submit", function () { dirtyEditor = null; });
  }

  window.addEventListener("beforeunload", function (e) {
    if (!dirtyEditor || !document.contains(dirtyEditor)) return;
    e.preventDefault();
    e.returnValue = "";
  });

  // htmx fires htmx:confirm before every request; hold any request that would
  // replace a dirty editor (other than its own save) until the user agrees.
  document.addEventListener("htmx:confirm", function (e) {
    if (!dirtyEditor || !document.contains(dirtyEditor)) return;
    var elt = e.detail && e.detail.elt;
    if (elt === dirtyEditor) return;
    e.preventDefault();
    var path = dirtyEditor.querySelector('input[name="path"]');
    var msg = "Discard unsaved changes to " + (path ? path.value : "this file") + "?";
    // issueRequest(true) skips the element's own hx-confirm, so ask both
    // questions in one prompt (e.g. deleting another file while editing).
    if (e.detail.question) msg = "You have unsaved changes to " + (path ? path.value : "this file") +
      ". Discard them and continue?\n\n" + e.detail.question;
    if (window.confirm(msg)) {
      dirtyEditor = null;
      e.detail.issueRequest(true);
    }
  });

  // ---------- Rename: select the name without its extension ----------

  function selectStem(input) {
    var dot = input.value.lastIndexOf(".");
    if (dot > 0) input.setSelectionRange(0, dot);
    else input.select();
  }

  function initSelectStem(input) {
    input.addEventListener("focus", function once() {
      input.removeEventListener("focus", once);
      selectStem(input);
    });
    if (document.activeElement === input) selectStem(input);
  }

  // ---------- Copy to clipboard ----------

  function isField(el) {
    return el.tagName === "INPUT" || el.tagName === "TEXTAREA";
  }

  function selectText(el) {
    if (isField(el)) {
      el.focus();
      el.select();
      return;
    }
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }

  // legacyCopy is for plain-http instances, where navigator.clipboard is
  // unavailable (it needs a secure context). On success focus returns to the
  // button; on failure the text stays selected so Ctrl/Cmd+C finishes the job.
  function legacyCopy(el, btn) {
    var ok = false;
    selectText(el);
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    if (ok) {
      if (!isField(el)) window.getSelection().removeAllRanges();
      btn.focus();
    }
    return ok;
  }

  function initCopy(btn) {
    var target = document.getElementById(btn.getAttribute("data-copy"));
    if (!target) return; // leave the button hidden: nothing to copy
    var label = btn.querySelector("[data-copy-label]") || btn;
    var original = label.textContent;
    var name = btn.getAttribute("data-copy-name") || "Text";
    var timer = null;

    function settle(ok) {
      clearTimeout(timer);
      // Restart the pop animation on repeated copies.
      btn.classList.remove("is-copied");
      void btn.offsetWidth;
      btn.classList.toggle("is-copied", ok);
      var region = document.querySelector("[data-copy-status]");
      if (ok) {
        label.textContent = "Copied";
        say(region, name + " copied to clipboard.");
      } else {
        label.textContent = isMac ? "Press ⌘C" : "Press Ctrl+C";
        say(region, "Couldn't copy automatically. " + name + " is selected; press " +
          (isMac ? "Command+C" : "Control+C") + " to copy it.");
      }
      timer = setTimeout(function () {
        label.textContent = original;
        btn.classList.remove("is-copied");
      }, ok ? 2000 : 4000);
    }

    btn.addEventListener("click", function () {
      var text = isField(target) ? target.value : target.textContent;
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(
          function () { settle(true); },
          function () { settle(legacyCopy(target, btn)); }
        );
      } else {
        settle(legacyCopy(target, btn));
      }
    });
    btn.hidden = false;
  }

  // ---------- Init ----------

  function once(el, key) {
    if (el.dataset[key] === "1") return false;
    el.dataset[key] = "1";
    return true;
  }

  function initAll(root) {
    root = root || document;
    var zones = root.querySelectorAll("[data-dropzone]");
    if (zones.length) installGlobalDropGuard();
    zones.forEach(function (zone) {
      if (!once(zone, "dzReady")) return;
      initDropzone(zone);
      if (zone.hasAttribute("data-autofill")) initAutofill(zone);
    });
    root.querySelectorAll("[data-copy]").forEach(function (btn) {
      if (once(btn, "copyReady")) initCopy(btn);
    });
    root.querySelectorAll("[data-slug-target]").forEach(function (el) {
      if (once(el, "slugReady")) initSlug(el);
    });
    root.querySelectorAll("[data-file-filter]").forEach(function (el) {
      if (once(el, "filterReady")) initFileFilter(el);
    });
    root.querySelectorAll("form[data-editor]").forEach(function (el) {
      if (once(el, "editorReady")) initEditor(el);
    });
    root.querySelectorAll("[data-select-stem]").forEach(function (el) {
      if (once(el, "stemReady")) initSelectStem(el);
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    initAll(document);
    cleanFlashParams();
    // Full page loads of the editor / rename views focus their field, as the
    // htmx path does after a swap (layout.html). Never steal focus from an
    // autofocus field or an error the page already focused.
    var main = document.getElementById("app-main");
    var target = main && main.querySelector("[data-autofocus]");
    if (target && (document.activeElement === document.body || !document.activeElement)) {
      target.focus();
    }
  });
  document.body.addEventListener("htmx:afterSettle", function () { initAll(document); });
})();
