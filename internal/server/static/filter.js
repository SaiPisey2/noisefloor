// Progressive enhancement only: the leaderboard table already renders and
// sorts (via the column-header links, server-side) with this disabled. All
// this adds is a client-side text filter over rule/group name.
(function () {
  "use strict";
  var input = document.getElementById("filter");
  var table = document.getElementById("leaderboard-table");
  if (!input || !table) return;

  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    var rows = table.tBodies[0].rows;
    for (var i = 0; i < rows.length; i++) {
      var name = (rows[i].getAttribute("data-name") || "").toLowerCase();
      rows[i].hidden = q !== "" && name.indexOf(q) === -1;
    }
  });
})();
