"use strict";

function initSourcesSortable() {
	const el = document.getElementById("sources-list");
	if (!el) return;
	new Sortable(el, {
		animation: 150,
		handle: ".drag-handle",
		multiDrag: true,
		selectedClass: "multiselected",
		onEnd: function () {
			htmx.trigger(document.getElementById("sources-form"), "submit");
		},
	});
}

document.body.addEventListener("click", function (e) {
	if (!e.target.closest("#sources-list") || e.target.closest("#sources-list > [data-kind]")) return;
	for (const el of document.querySelectorAll("#sources-list .multiselected")) {
		Sortable.utils.deselect(el);
	}
});

function collectLayout() {
	const items = [];
	const seen = new Set();
	for (const el of document.querySelectorAll("#sources-list > [data-kind]")) {
		const key = el.dataset.kind === "section" ? "section:" + el.dataset.id : "source:" + el.dataset.type + ":" + el.dataset.number;
		if (seen.has(key)) continue;
		seen.add(key);
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

document.body.addEventListener("htmx:configRequest", function (e) {
	if (e.detail.elt && e.detail.elt.id === "sources-form") {
		e.detail.parameters.layout = JSON.stringify(collectLayout());
	}
});

document.addEventListener("DOMContentLoaded", initSourcesSortable);

function syncLayoutURL() {
	const hidden = document.querySelector("#sources-form input[name='layoutId']");
	if (!hidden) return;
	const url = new URL(location.href);
	url.searchParams.set("layout", hidden.value);
	history.replaceState(null, "", url);
}

function onSourcesSwap(e) {
	if (e.detail.target && e.detail.target.id === "section-sources") {
		initSourcesSortable();
		syncLayoutURL();
	}
}
document.body.addEventListener("htmx:afterSwap", onSourcesSwap);
document.body.addEventListener("htmx:oobAfterSwap", onSourcesSwap);

let lastDeskStatus = null;
document.body.addEventListener("htmx:afterSwap", function (e) {
	if (!e.detail.target || e.detail.target.id !== "desk-status") return;
	const dot = e.detail.target.querySelector(".status-dot");
	const status = dot && dot.dataset.status;
	if (lastDeskStatus === null) {
		lastDeskStatus = status;
	} else if (status !== lastDeskStatus) {
		location.reload();
	}
});

const POPUP_SIZES = {
	"icon-popup": { width: 240, height: 256 },
	"preset-menu": { width: 150, height: 160 },
};

document.addEventListener("beforetoggle", function (e) {
	const sizeClass = Object.keys(POPUP_SIZES).find((c) => e.target.classList.contains(c));
	if (!sizeClass || e.newState !== "open") return;
	const btn = document.querySelector(`[popovertarget="${e.target.id}"]`);
	if (!btn) return;
	const rect = btn.getBoundingClientRect();
	const { width, height } = POPUP_SIZES[sizeClass];
	const popup = e.target;
	popup.style.position = "fixed";
	popup.style.inset = "auto";
	popup.style.margin = "0";
	popup.style.left = Math.min(rect.left, window.innerWidth - width - 8) + "px";
	popup.style.top = Math.min(rect.bottom + 4, window.innerHeight - height - 8) + "px";
}, true);

document.body.addEventListener("change", function (e) {
	if (e.target.closest("#sources-form") && (e.target.classList.contains("enabled-field") || e.target.classList.contains("section-name-field"))) {
		if (e.target.classList.contains("enabled-field") && e.target.closest(".multiselected")) {
			for (const other of document.querySelectorAll("#sources-list .multiselected .enabled-field")) {
				other.checked = e.target.checked;
			}
		}
		htmx.trigger(document.getElementById("sources-form"), "submit");
	} else if (e.target.closest("#auxes-form")) {
		htmx.trigger(document.getElementById("auxes-form"), "submit");
	} else if (e.target.closest("#presets-upload-form")) {
		htmx.trigger(document.getElementById("presets-upload-form"), "submit");
	}
});

function toggleAllEnabled(scopeSelector, checkboxSelector) {
	const boxes = document.querySelectorAll(scopeSelector + " " + checkboxSelector);
	if (boxes.length === 0) return;
	const allChecked = [...boxes].every((b) => b.checked);
	for (const b of boxes) b.checked = !allChecked;
}

document.body.addEventListener("click", function (e) {
	const opt = e.target.closest(".icon-option");
	if (opt) {
		const picker = opt.closest(".icon-picker");
		picker.querySelector(".icon-value").value = opt.dataset.icon;
		picker.querySelector(".icon-preview").src = opt.dataset.icon ? "/admin/icons/" + opt.dataset.icon : "";
		for (const o of picker.querySelectorAll(".icon-option")) o.classList.toggle("selected", o === opt);
		opt.closest(".icon-popup").hidePopover();
		htmx.trigger(document.getElementById("auxes-form"), "submit");
		return;
	}
	if (e.target.id === "sources-toggle-all") {
		toggleAllEnabled("#sources-list", ".enabled-field");
		htmx.trigger(document.getElementById("sources-form"), "submit");
	} else if (e.target.id === "auxes-toggle-all") {
		toggleAllEnabled("#section-auxes", "input[name^='enabled[']");
		htmx.trigger(document.getElementById("auxes-form"), "submit");
	}
});

document.body.addEventListener("click", function (e) {
	const btn = e.target.closest(".add-section-btn");
	if (!btn) return;
	const layoutIdField = document.querySelector("#sources-form [name='layoutId']");
	htmx.ajax("POST", "/admin/sections", {
		target: "#section-sources",
		swap: "outerHTML",
		values: {
			name: "New section",
			beforeKey: btn.dataset.beforeKey || "",
			layoutId: layoutIdField ? layoutIdField.value : "",
		},
	});
});

document.body.addEventListener("click", function (e) {
	if (e.target.id !== "add-device-toggle") return;
	const form = document.querySelector(".add-device");
	if (!form) return;
	form.hidden = false;
	e.target.hidden = true;
});

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
