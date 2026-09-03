"use strict";

// Regression coverage for mixer.js's layoutFor()/buildSources() chain — the
// mainline client render path (selectAux/buildAux both do
// buildSources(layoutFor(aux))). This path shipped fully broken (threw a
// TypeError on every invocation) until a late fix in this branch's
// history, caught only by a one-off manual Node harness that was never
// saved to the repo. This file is that harness, saved.
//
// mixer.js is a plain browser script (IIFE, no module.exports, no build
// step) — layoutFor/buildSources/mixerLayouts are private to its closure.
// Rather than reimplementing them (which risks drifting from the shipped
// code) or adding a module.exports guard to the shipped file (which would
// touch the browser code path to serve a test), this loads the IIFE's
// actual body text and runs it with Node's vm module in a sandboxed
// context that stubs just enough of `document` for module-scope
// evaluation (document.getElementById/addEventListener/createElement) to
// succeed without a real DOM. The functions under test are then called
// directly out of that context — this is the real, unmodified mixer.js
// source, not a reimplementation.
//
// Run: node --test web/mixer/mixer_test.js  (Node 18+, no dependencies)

const test = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const fs = require("node:fs");
const path = require("node:path");

const source = fs.readFileSync(path.join(__dirname, "mixer.js"), "utf8");

// Slice out the IIFE's body: everything between "(() => {" and the final
// "})();" — the actual statements that declare mixerLayouts/layoutFor/
// buildSources/etc. Running just this body (not the whole file, which
// wraps it in another function scope) as vm top-level code is what makes
// those top-level `let`/`function` bindings reachable afterward.
const start = source.indexOf("(() => {");
const end = source.lastIndexOf("})();");
assert.ok(start !== -1 && end !== -1 && end > start, "could not locate mixer.js's IIFE body");
const body = source.slice(start + "(() => {".length, end);

// Minimal DOM stub — just enough for the module-scope code (8x
// document.getElementById, one document.addEventListener("DOMContentLoaded",
// ...) call, and escapeHtml's document.createElement) to run without
// throwing. Nothing here needs to render real HTML; buildSources's own
// data (the `sources` variable it sets from the layout) is what the tests
// assert on, not the innerHTML string.
function stubElement() {
	let html = "";
	return {
		get innerHTML() { return html; },
		set innerHTML(v) { html = v; },
		querySelectorAll: () => [],
		addEventListener: () => {},
		classList: { add() {}, remove() {}, toggle() {} },
		style: { setProperty() {} },
	};
}

function stubDocument() {
	return {
		getElementById: () => stubElement(),
		addEventListener: () => {},
		createElement: () => {
			let text = "";
			return {
				set textContent(v) { text = v == null ? "" : String(v); },
				get innerHTML() {
					return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
				},
			};
		},
	};
}

// loadMixerModule runs a fresh copy of mixer.js's IIFE body in its own vm
// context, so each test starts with clean module-level state (mixerLayouts
// = {}, sources = [], etc.) rather than leaking between tests.
function loadMixerModule() {
	const sandbox = { document: stubDocument() };
	vm.createContext(sandbox);
	vm.runInContext(body, sandbox, { filename: "mixer.js (IIFE body)" });
	return {
		// run evaluates an expression/statement in the already-initialized
		// context — top-level `let`/`function` bindings from `body` are
		// visible here because vm shares the lexical global environment
		// across runInContext calls on the same context.
		run: (code) => vm.runInContext(code, sandbox),
	};
}

test("layoutFor: aux with a valid custom layoutId resolves that layout, buildSources doesn't throw", () => {
	const mod = loadMixerModule();
	mod.run(`mixerLayouts = ${JSON.stringify({
		default: { name: "Default", items: [{ kind: "source", type: "channel", number: 1, label: "Kick", enabled: true }] },
		custom1: {
			name: "Monitor Mix",
			items: [
				{ kind: "section", sectionName: "Drums" },
				{ kind: "source", type: "channel", number: 2, label: "Snare", enabled: true },
				{ kind: "source", type: "channel", number: 3, label: "Hats", enabled: true },
			],
		},
	})}`);
	const items = mod.run(`layoutFor({ channel: 1, layoutId: "custom1" })`);
	assert.equal(items.length, 3);
	assert.equal(items[1].label, "Snare");
	assert.equal(items[2].label, "Hats");

	assert.doesNotThrow(() => mod.run(`buildSources(layoutFor({ channel: 1, layoutId: "custom1" }))`));
	const sources = mod.run(`sources`);
	assert.equal(sources.length, 2); // section marker filtered out, 2 source entries remain
	// Array.from copies into a host-realm array — sources itself is a
	// vm-realm array, and deepStrictEqual treats cross-realm arrays as
	// different types even with identical primitive contents.
	assert.deepEqual(Array.from(sources, (s) => s.label), ["Snare", "Hats"]);
});

test('layoutFor: aux with layoutId "" resolves to the default layout', () => {
	const mod = loadMixerModule();
	mod.run(`mixerLayouts = ${JSON.stringify({
		default: { name: "Default", items: [{ kind: "source", type: "channel", number: 1, label: "Kick", enabled: true }] },
	})}`);
	const items = mod.run(`layoutFor({ channel: 1, layoutId: "" })`);
	assert.equal(items.length, 1);
	assert.equal(items[0].label, "Kick");
});

test('layoutFor: aux with layoutId "default" resolves to the default layout', () => {
	const mod = loadMixerModule();
	mod.run(`mixerLayouts = ${JSON.stringify({
		default: { name: "Default", items: [{ kind: "source", type: "channel", number: 1, label: "Kick", enabled: true }] },
	})}`);
	const items = mod.run(`layoutFor({ channel: 1, layoutId: "default" })`);
	assert.equal(items.length, 1);
	assert.equal(items[0].label, "Kick");
});

test("layoutFor: aux with a dangling layoutId (deleted layout) falls back to default, buildSources doesn't throw", () => {
	const mod = loadMixerModule();
	mod.run(`mixerLayouts = ${JSON.stringify({
		default: { name: "Default", items: [{ kind: "source", type: "channel", number: 1, label: "Kick", enabled: true }] },
	})}`);
	const items = mod.run(`layoutFor({ channel: 1, layoutId: "deleted-layout-id" })`);
	assert.equal(items.length, 1);
	assert.equal(items[0].label, "Kick");

	assert.doesNotThrow(() => mod.run(`buildSources(layoutFor({ channel: 1, layoutId: "deleted-layout-id" }))`));
	const sources = mod.run(`sources`);
	assert.equal(sources.length, 1);
	assert.equal(sources[0].label, "Kick");
});

test("layoutFor: null/undefined aux (no aux selected yet) falls back to default, buildSources doesn't throw", () => {
	const mod = loadMixerModule();
	mod.run(`mixerLayouts = ${JSON.stringify({
		default: { name: "Default", items: [{ kind: "source", type: "channel", number: 1, label: "Kick", enabled: true }] },
	})}`);

	const itemsNull = mod.run(`layoutFor(null)`);
	assert.equal(itemsNull.length, 1);
	assert.equal(itemsNull[0].label, "Kick");

	const itemsUndefined = mod.run(`layoutFor(undefined)`);
	assert.equal(itemsUndefined.length, 1);
	assert.equal(itemsUndefined[0].label, "Kick");

	assert.doesNotThrow(() => mod.run(`buildSources(layoutFor(null))`));
	const sources = mod.run(`sources`);
	assert.equal(sources.length, 1);
	assert.equal(sources[0].label, "Kick");
});
