// Volume file browser (volume_files.html): upload files with a progress bar,
// and close the header menus once their action changed the table.
(function () {
  var form = document.querySelector("form[data-volume-file-upload]");
  if (form && window.XMLHttpRequest && window.FormData) {
    var box = form.querySelector("[data-upload-progress]");
    var bar = box.querySelector("progress");
    var status = form.querySelector("[data-upload-status]");
    var errBox = form.querySelector("[data-upload-error]");
    var btn = form.querySelector('button[type="submit"]');
    var input = form.querySelector('input[type="file"]');
    var fail = function (msg) {
      errBox.textContent = msg;
      errBox.hidden = false;
      box.hidden = true;
      btn.disabled = false;
    };
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      if (!input.files.length) return;
      var url = form.getAttribute("data-url");
      if (form.elements.namedItem("overwrite").checked) url += (url.indexOf("?") < 0 ? "?" : "&") + "overwrite=1";
      var data = new FormData();
      for (var i = 0; i < input.files.length; i++) data.append("files", input.files[i], input.files[i].name);
      var xhr = new XMLHttpRequest();
      xhr.open("POST", url);
      // The token goes in a header so that the server reads the body
      // only once the upload may take as long as it needs.
      xhr.setRequestHeader("X-CSRF-Token", form.getAttribute("data-csrf"));
      xhr.setRequestHeader("Accept", "application/json");
      xhr.upload.addEventListener("progress", function (ev) {
        if (!ev.lengthComputable) return;
        bar.max = ev.total;
        bar.value = ev.loaded;
        status.textContent = Math.floor(ev.loaded * 100 / ev.total) + " % uploaded";
      });
      xhr.upload.addEventListener("load", function () {
        bar.removeAttribute("value");
        status.textContent = "Writing into the volume…";
      });
      xhr.addEventListener("load", function () {
        var res = null;
        try { res = JSON.parse(xhr.responseText); } catch (_) { /* not JSON */ }
        if (res && !res.error && res.redirect) {
          window.location.href = res.redirect;
          return;
        }
        fail(res && res.error ? res.error : "The upload failed (" + xhr.status + ").");
        if (res && res.uploaded > 0) document.body.dispatchEvent(new Event("volume-files-changed"));
      });
      xhr.addEventListener("error", function () { fail("The upload failed: the server could not be reached."); });
      errBox.hidden = true;
      box.hidden = false;
      bar.removeAttribute("value");
      status.textContent = "";
      btn.disabled = true;
      xhr.send(data);
    });
  }

  // A successful action from a header menu swaps the table: close the menu
  // and clear its form.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (!e.detail.target || e.detail.target.id !== "volume-files") return;
    var table = document.getElementById("volume-files");
    if (table && table.querySelector(".flash.error")) return;
    document.querySelectorAll(".page-head details.menu[open]").forEach(function (d) {
      d.open = false;
      var f = d.querySelector("form");
      if (f && !f.hasAttribute("data-volume-file-upload")) f.reset();
    });
  });
})();
