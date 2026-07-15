// app.js — progressive enhancement for the admin UI. No external dependencies.
//
// Dropzone: a drag-and-drop upload surface with a staged file list. Dropping or
// browsing ACCUMULATES files (it does not replace the previous selection), each
// staged file gets a remove button, and the real <input type="file"> is kept in
// sync so a normal form submit uploads exactly the staged set. Without
// JavaScript the bare input still works (browse + native drop), just without
// accumulation or the removable list.
//
// Autofill: on the new-project form, staging a single HTML file fills the Name
// and Slug fields from the file name, unless the user has typed there already.
//
// Init runs on DOMContentLoaded and after every htmx swap, guarded so each
// dropzone is wired exactly once.
(function () {
  "use strict";

  function slugify(s) {
    return String(s)
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
    var staged = new DataTransfer();
    var syncing = false;

    function emitChange() {
      zone.dispatchEvent(new CustomEvent("dz:change"));
    }

    function mirror() {
      // Assigning input.files does not fire "change"; the guard is belt-and-braces.
      syncing = true;
      input.files = staged.files;
      syncing = false;
      renderList();
      zone.classList.toggle("dropzone--filled", staged.files.length > 0);
      emitChange();
    }

    function addFiles(fileList) {
      if (!fileList || !fileList.length) return;
      // Merge by base name (case-insensitive), preserving order. A newer file
      // with an existing name replaces the staged one — the server stores by
      // base name, so one entry per filename keeps staging and result in step.
      var byName = {}, order = [];
      function put(f) {
        var k = f.name.toLowerCase();
        if (!(k in byName)) order.push(k);
        byName[k] = f;
      }
      for (var i = 0; i < staged.files.length; i++) put(staged.files[i]);
      for (var j = 0; j < fileList.length; j++) put(fileList[j]);
      staged = new DataTransfer();
      order.forEach(function (k) { staged.items.add(byName[k]); });
      mirror();
    }

    function removeAt(idx) {
      var kept = [];
      for (var i = 0; i < staged.files.length; i++) if (i !== idx) kept.push(staged.files[i]);
      staged = new DataTransfer();
      kept.forEach(function (f) { staged.items.add(f); });
      mirror();
    }

    function clearAll() {
      staged = new DataTransfer();
      mirror();
    }

    function renderList() {
      if (!listEl) return;
      listEl.textContent = "";
      var n = staged.files.length;
      listEl.hidden = n === 0;
      if (n === 0) return;

      var head = document.createElement("div");
      head.className = "dz-files__head";
      var count = document.createElement("span");
      count.textContent = n + (n === 1 ? " file selected" : " files selected");
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
      for (var i = 0; i < n; i++) {
        (function (idx, f) {
          var li = document.createElement("li");
          li.className = "dz-file";
          var name = document.createElement("span");
          name.className = "dz-file__name";
          name.textContent = f.name;
          var size = document.createElement("span");
          size.className = "dz-file__size";
          size.textContent = humanBytes(f.size);
          var rm = document.createElement("button");
          rm.type = "button"; // never submit the form
          rm.className = "dz-file__remove";
          rm.setAttribute("aria-label", "Remove " + f.name);
          rm.textContent = "×";
          rm.addEventListener("click", function () { removeAt(idx); });
          li.appendChild(name);
          li.appendChild(size);
          li.appendChild(rm);
          ul.appendChild(li);
        })(i, staged.files[i]);
      }
      listEl.appendChild(ul);
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
      if (slugEl) slugEl.value = slugify(pending);
      (nameEl || slugEl).focus();
    });

    zone.addEventListener("dz:change", refresh);
    refresh();
  }

  function initAll(root) {
    var zones = (root || document).querySelectorAll("[data-dropzone]");
    if (zones.length) installGlobalDropGuard();
    zones.forEach(function (zone) {
      if (zone.dataset.dzReady === "1") return;
      zone.dataset.dzReady = "1";
      initDropzone(zone);
      if (zone.hasAttribute("data-autofill")) initAutofill(zone);
    });
  }

  document.addEventListener("DOMContentLoaded", function () { initAll(document); });
  document.body.addEventListener("htmx:afterSettle", function () { initAll(document); });
})();
