// Compose editor: open or drop a compose file into the textarea, indent with
// Tab, and jump to the line of a reported problem. Validation itself runs on
// the server (htmx on the textarea).
(function () {
  var form = document.querySelector("[data-compose-form]");
  if (!form) return;
  var yaml = form.querySelector("[data-compose-yaml]");
  var project = form.querySelector("[data-compose-project]");
  var fileInput = form.querySelector("[data-compose-file]");
  var drop = form.querySelector("[data-compose-drop]");
  var maxSize = 512 * 1024;

  // A project name from a file name: "shop.compose.yaml" -> "shop". The
  // generic names (compose.yaml, docker-compose.yml) say nothing.
  function nameFromFile(name) {
    var base = name.replace(/\.(ya?ml)$/i, "").replace(/(^|[.-])(docker-)?compose$/i, "").toLowerCase();
    base = base.replace(/[^a-z0-9_-]+/g, "-").replace(/^[^a-z0-9]+|-+$/g, "");
    return base;
  }

  function load(file) {
    if (!file) return;
    if (file.size > maxSize) {
      window.alert(file.name + " is larger than 512 KiB.");
      return;
    }
    var reader = new FileReader();
    reader.onload = function () {
      yaml.value = String(reader.result).replace(/\r\n/g, "\n");
      if (project && !project.readOnly && project.value === "") {
        project.value = nameFromFile(file.name);
      }
      // Let htmx validate the new content.
      yaml.dispatchEvent(new Event("input", { bubbles: true }));
      yaml.focus();
    };
    reader.readAsText(file);
  }

  form.querySelector("[data-compose-open]").addEventListener("click", function () {
    fileInput.click();
  });
  fileInput.addEventListener("change", function () {
    load(fileInput.files[0]);
    fileInput.value = "";
  });

  var depth = 0; // dragenter/dragleave also fire for child elements
  drop.addEventListener("dragenter", function (e) {
    e.preventDefault();
    depth++;
    drop.classList.add("dragging");
  });
  drop.addEventListener("dragover", function (e) {
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  });
  drop.addEventListener("dragleave", function () {
    if (--depth <= 0) {
      depth = 0;
      drop.classList.remove("dragging");
    }
  });
  drop.addEventListener("drop", function (e) {
    e.preventDefault();
    depth = 0;
    drop.classList.remove("dragging");
    if (e.dataTransfer.files.length > 0) load(e.dataTransfer.files[0]);
  });

  // Tab indents by two spaces. Escape first to leave the editor with Tab.
  var escaped = false;
  yaml.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      escaped = true;
      return;
    }
    if (e.key !== "Tab" || e.shiftKey || e.ctrlKey || e.altKey || e.metaKey || escaped) {
      escaped = false;
      return;
    }
    e.preventDefault();
    var start = yaml.selectionStart;
    yaml.setRangeText("  ", start, yaml.selectionEnd, "end");
    yaml.dispatchEvent(new Event("input", { bubbles: true }));
  });

  // Clicking "line N" in the problem list selects that line.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-compose-line]");
    if (!btn) return;
    var line = parseInt(btn.getAttribute("data-compose-line"), 10);
    var lines = yaml.value.split("\n");
    if (!(line >= 1 && line <= lines.length)) return;
    var start = 0;
    for (var i = 0; i < line - 1; i++) start += lines[i].length + 1;
    yaml.focus();
    yaml.setSelectionRange(start, start + lines[line - 1].length);
    var lineHeight = parseFloat(getComputedStyle(yaml).lineHeight) || 20;
    yaml.scrollTop = Math.max(0, (line - 4) * lineHeight);
  });
})();
