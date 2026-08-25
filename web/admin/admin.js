"use strict";

// Drag-and-drop reordering for the sources table. Kept separate from htmx's
// own request handling — SortableJS owns the drag interaction, and its
// onEnd callback does a plain fetch() to persist the new order, then
// swaps the returned sources-section fragment back in. Must be re-run
// after ANY htmx swap that replaces #sources-tbody (a per-row save, or
// this reorder itself), since outerHTML replacement creates a brand new
// DOM node with no Sortable instance attached.
function initSourcesSortable() {
	const el = document.getElementById("sources-tbody");
	if (!el) return;
	new Sortable(el, {
		animation: 150,
		handle: ".drag-handle",
		onEnd: onSourcesReordered,
	});
}

function onSourcesReordered() {
	const order = [...document.querySelectorAll("#sources-tbody tr")]
		.map((tr) => tr.dataset.key)
		.join(",");

	fetch("/admin/sources/reorder", {
		method: "POST",
		headers: { "Content-Type": "application/x-www-form-urlencoded" },
		body: "order=" + encodeURIComponent(order),
	})
		.then((r) => r.text())
		.then((html) => {
			const section = document.getElementById("section-sources");
			if (section) {
				section.outerHTML = html;
				initSourcesSortable();
			}
		});
}

document.addEventListener("DOMContentLoaded", initSourcesSortable);

// Re-init after any htmx swap that touched the sources section (a row
// save, or an out-of-band update from a preset load).
document.body.addEventListener("htmx:afterSwap", function (e) {
	if (e.detail.target && e.detail.target.id === "section-sources") {
		initSourcesSortable();
	}
});
document.body.addEventListener("htmx:oobAfterSwap", function (e) {
	if (e.detail.target && e.detail.target.id === "section-sources") {
		initSourcesSortable();
	}
});
