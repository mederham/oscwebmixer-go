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
				icon: el.querySelector(".icon-field").value,
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
