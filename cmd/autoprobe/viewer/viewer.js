// Lightweight client behaviors for the trace viewer. No dependencies.
// Each iteration page embeds prev/next URLs (if any) on document.body's
// data-attributes; we wire ←/→/j/k/Esc to navigate. Sortable tables read
// data-sort-key/data-sort-type from headers and reorder the tbody rows.

(function () {
  function navAttr(name) {
    return document.body.getAttribute("data-" + name) || "";
  }

  function bindKeys() {
    document.addEventListener("keydown", function (e) {
      // Ignore when the user is typing in an editable field.
      const t = e.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;

      const prev = navAttr("prev-href");
      const next = navAttr("next-href");
      const index = navAttr("index-href");

      switch (e.key) {
        case "ArrowLeft":
        case "h":
        case "k":
          if (prev) { e.preventDefault(); window.location.href = prev; }
          break;
        case "ArrowRight":
        case "l":
        case "j":
          if (next) { e.preventDefault(); window.location.href = next; }
          break;
        case "Escape":
        case "u":
          if (index) { e.preventDefault(); window.location.href = index; }
          break;
        case "g":
          // jump to iteration prompt
          if (index) {
            e.preventDefault();
            const n = window.prompt("Jump to iteration #:");
            if (!n) return;
            const num = parseInt(n, 10);
            if (!isFinite(num) || num < 1) return;
            const padded = String(num).padStart(5, "0");
            window.location.href = "iter-" + padded + ".html";
          }
          break;
      }
    });
  }

  function makeSortable(table) {
    const tbody = table.tBodies[0];
    if (!tbody) return;
    const headers = table.tHead ? Array.from(table.tHead.rows[0].cells) : [];
    headers.forEach(function (th, colIdx) {
      const key = th.getAttribute("data-sort-key");
      if (!key) return;
      const type = th.getAttribute("data-sort-type") || "string";
      // Add indicator span if not present.
      if (!th.querySelector(".sort-indicator")) {
        const ind = document.createElement("span");
        ind.className = "sort-indicator";
        ind.textContent = "↕";
        th.appendChild(document.createTextNode(" "));
        th.appendChild(ind);
      }
      th.addEventListener("click", function () {
        const dir = th.classList.contains("sorted-asc") ? "desc" : "asc";
        Array.from(headers).forEach(function (h) {
          h.classList.remove("sorted", "sorted-asc", "sorted-desc");
          const ind = h.querySelector(".sort-indicator");
          if (ind) ind.textContent = "↕";
        });
        th.classList.add("sorted", "sorted-" + dir);
        const ind = th.querySelector(".sort-indicator");
        if (ind) ind.textContent = dir === "asc" ? "↑" : "↓";

        const rows = Array.from(tbody.rows);
        rows.sort(function (a, b) {
          const av = cellValue(a.cells[colIdx], type);
          const bv = cellValue(b.cells[colIdx], type);
          if (av < bv) return dir === "asc" ? -1 : 1;
          if (av > bv) return dir === "asc" ? 1 : -1;
          return 0;
        });
        rows.forEach(function (r) { tbody.appendChild(r); });
      });
    });
  }

  function cellValue(cell, type) {
    if (!cell) return type === "number" ? -Infinity : "";
    const raw = cell.getAttribute("data-sort-value");
    const v = raw !== null ? raw : cell.textContent.trim();
    if (type === "number") {
      const n = parseFloat(v);
      return isFinite(n) ? n : -Infinity;
    }
    if (type === "bool") {
      return v === "true" || v === "1" ? 1 : 0;
    }
    return v.toLowerCase();
  }

  function init() {
    bindKeys();
    Array.from(document.querySelectorAll("table.sortable")).forEach(makeSortable);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
