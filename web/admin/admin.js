"use strict";

// Drag-and-drop for the Sources tab — one flat list mixing source cards
// and section markers (see internal/oscmsg.LayoutItem's doc comment: a
// source's section is derived from whichever marker precedes it in this
// same list, not a stored field, so there's nothing to keep in sync beyond
// the list's own order — no groups, no nested containers, just one
// Sortable instance). Every drop re-submits the whole list via
// #sources-form, same as the visible Save button.
function initSourcesSortable() {
	const el = document.getElementById("sources-list");
	if (!el) return;
	new Sortable(el, {
		animation: 150,
		handle: ".drag-handle",
		onEnd: function () {
			htmx.trigger(document.getElementById("sources-form"), "submit");
		},
	});
}

// Reads the current DOM state of #sources-list (order + every field's live
// value) into the wire shape internal/oscmsg.LayoutItem expects. Called
// fresh on every submission — Save-button click or drag-drop auto-persist
// alike — via htmx:configRequest below, never cached, so it's always
// exactly what's on screen.
function collectLayout() {
	const items = [];
	for (const el of document.querySelectorAll("#sources-list > [data-kind]")) {
		if (el.dataset.kind === "section") {
			items.push({
				kind: "section",
				sectionId: el.dataset.id,
				sectionName: el.querySelector(".section-name-field").value,
			});
		} else {
			items.push({
				kind: "source",
				type: el.dataset.type,
				number: parseInt(el.dataset.number, 10),
				enabled: el.querySelector(".enabled-field").checked,
			});
		}
	}
	return items;
}

// Injects the live layout JSON into #sources-form's request right before
// it's sent — htmx's documented extension point for computing a request's
// parameters, used here instead of named per-field inputs since the
// browser's native form serialization has no way to represent an ordered,
// mixed-kind list (see internal/admin's package doc comment). Everything
// else about the request/response/swap lifecycle is still plain htmx.
document.body.addEventListener("htmx:configRequest", function (e) {
	if (e.detail.elt && e.detail.elt.id === "sources-form") {
		e.detail.parameters.layout = JSON.stringify(collectLayout());
	}
});

document.addEventListener("DOMContentLoaded", initSourcesSortable);

// Re-init after any htmx swap that touched the sources section (a Save, a
// drag-drop auto-save, a section add/delete, or an out-of-band update from
// a preset load) — outerHTML replacement creates brand new DOM nodes with
// no Sortable instance attached.
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

// Aux icon picker: the <select> already lists every option server-side
// (see auxes.html.tmpl), so this is just keeping the adjacent preview
// image in sync with whatever's currently selected — event delegation on
// document.body means it works after any htmx swap of the auxes section
// too, no re-init needed.
document.body.addEventListener("change", function (e) {
	if (e.target.classList.contains("icon-select")) {
		const img = e.target.parentElement.querySelector(".icon-preview");
		if (img) img.src = e.target.value ? "/admin/icons/" + e.target.value : "";
		return;
	}

	// Sources' enabled toggle and section-rename field both apply
	// immediately, same as dragging to reorder — leaving any of them
	// behind a Save button (as auxes' fields still are) meant different
	// edits on the same tab behaved inconsistently. "change" (not "blur")
	// on a text input only fires when the value actually differs from
	// what it was on focus, so tabbing through without editing is a
	// no-op, not a wasted save. Auxes has no drag-and-drop to be
	// inconsistent with, so its fields stay batched behind Save.
	if (e.target.classList.contains("enabled-field") || e.target.classList.contains("section-name-field")) {
		htmx.trigger(document.getElementById("sources-form"), "submit");
	}
});

// "Show/Hide all": a single smart toggle per tab — if everything in scope
// is currently enabled, disable all; otherwise enable all.
function toggleAllEnabled(scopeSelector, checkboxSelector) {
	const boxes = document.querySelectorAll(scopeSelector + " " + checkboxSelector);
	if (boxes.length === 0) return;
	const allChecked = [...boxes].every((b) => b.checked);
	for (const b of boxes) b.checked = !allChecked;
}

document.body.addEventListener("click", function (e) {
	if (e.target.id === "sources-toggle-all") {
		toggleAllEnabled("#sources-list", ".enabled-field");
		// Setting .checked in JS doesn't fire "change", so this toggle
		// needs its own explicit submit to match individual checkboxes
		// now applying immediately (see the change listener above).
		htmx.trigger(document.getElementById("sources-form"), "submit");
	} else if (e.target.id === "auxes-toggle-all") {
		// Auxes stays batched behind its own Save button, unchanged.
		toggleAllEnabled("#section-auxes", "input[name^='enabled[']");
	}
});

// Tabs: plain hide/show over server-rendered content that's already all in
// the DOM (no fetching, no imperative element construction) — every
// section still renders and swaps exactly as it does today, this just
// controls which one is visible. location.hash makes the current tab
// survive a reload or a shared link, mirroring the legacy admin's nav
// pattern minus its per-field DOM-construction/move-up-down mess.
const tabLinks = document.querySelectorAll(".tabs a");

function showTab(hash) {
	for (const link of tabLinks) {
		link.classList.toggle("selected", link.getAttribute("href") === hash);
	}
	for (const panel of document.querySelectorAll(".tab-panel")) {
		panel.classList.toggle("show", panel.id === "tab-" + hash.slice(1));
	}
}

for (const link of tabLinks) {
	link.addEventListener("click", function (e) {
		e.preventDefault();
		const hash = this.getAttribute("href");
		history.replaceState(null, "", hash);
		showTab(hash);
	});
}

if (document.location.hash) {
	showTab(document.location.hash);
}
