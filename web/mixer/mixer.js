"use strict";

(() => {

const channelsDiv = document.getElementById('channels'),
auxPickerDiv = document.getElementById('aux-picker'),
auxTrigger = document.getElementById('aux-trigger'),
auxIcon = document.getElementById('aux-icon'),
auxLabel = document.getElementById('aux-label'),
panToggleWrap = document.getElementById('pan-toggle-wrap'),
panToggle = document.getElementById('pan-toggle'),
snapshotSpan = document.getElementById('snapshot');

let ws = null,
reconnectTimer = null,
sources = [],
auxes = [],
mixerLayouts = {},
selectedAux = null; // the full aux object (channel/label/colour/icon/stereo/layoutId), not just an id

const panSnapThreshold = 0.03; // +/- around 0.5 (center) that snaps to exact center — see sliderChange
const defaultTint = "74,144,217"; // DiGiCo blue, r,g,b — matches mixer.css's :root default; used when an aux has no/unparseable colour

function sliderToDb(value) {
	const val = ((Math.log(value * 100) / Math.log(100)) * 100) - 90;
	if (val === -Infinity) return -150;
	return val;
}

function dbToSlider(db) {
	return Math.pow(100, (db + 90) / 100) / 100;
}

function sourceKey(type, number) {
	return type + ":" + number;
}

// Source-type-aware OSC address root — a channel and a group bus both send
// to an aux via the same Aux_Send/N/send_level|send_pan shape, just under
// different namespace roots (see plan: "Group buses").
function addressRoot(type) {
	return type === "group" ? "/Group_Outputs" : "/Input_Channels";
}

// layoutFor resolves which named layout an aux renders — an aux with no
// layoutId (or one naming a layout that's since been deleted admin-side)
// falls back to "default", mirroring config.Aux.LayoutID's doc comment.
function layoutFor(aux) {
	return (aux && mixerLayouts[aux.layoutId]) || mixerLayouts["default"] || [];
}

// hexToRgb turns admin's "#rrggbb" colour picker output into the "r,g,b"
// triplet CSS custom properties need (rgb()/rgba() don't accept a hex
// string through var()) — null on anything unparseable, so callers fall
// back to the CSS default instead of setting a broken custom property.
function hexToRgb(hex) {
	const m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex || "");
	if (!m) return null;
	return parseInt(m[1], 16) + "," + parseInt(m[2], 16) + "," + parseInt(m[3], 16);
}

function iconSrc(icon) {
	return icon ? "/admin/icons/" + encodeURIComponent(icon) : "";
}

function sendOSC(address, args) {
	if (!ws || ws.readyState !== WebSocket.OPEN) return;
	ws.send(JSON.stringify({
		kind: "osc",
		message: { address: address, args: args || [] }
	}));
}

// sendThrottleMs matches the desk's own fader-update cadence — captured
// live, DigiCo echoes a moving fader's value at ~30-40Hz best-effort
// regardless of what's driving it (its own iPad app or this web mixer).
// Sending faster than that just adds load (WS + UDP + hub processing per
// message) without adding resolution the desk itself ever produces.
const sendThrottleMs = 30;

// dragEchoGuardMs bridges the round trip a locally-sent value takes before
// it comes back as a server push: our own throttle delay (sendThrottleMs)
// plus the desk's own echo latency. Without this, the desk's echo of a
// value THIS client just sent — a beat old by the time it arrives — lands
// back on this same screen and overwrites the slider mid-drag, fighting
// the user's live input (confirmed live: visible fighting/jitter while
// dragging). While active `input` events keep arriving, each one re-arms
// the guard well before it can expire, so it only actually lapses once
// the user has stopped moving the slider.
const dragEchoGuardMs = 300;
const sliderSendState = new WeakMap(); // el -> {timer, lastSent, pending, dragUntil}

// isDraggingLocally reports whether el is (or very recently was) being
// dragged by this client — see dragEchoGuardMs's doc comment for why
// onMessage uses this to skip applying a server push while it's true.
function isDraggingLocally(el) {
	const state = sliderSendState.get(el);
	return !!state && performance.now() < state.dragUntil;
}

// throttledSliderSend coalesces a fast-dragging slider's sends to at most
// one per sendThrottleMs, always the latest value — never a queue of
// stale ones (see sendThrottleMs's doc comment for why this cap exists,
// and the "external devices" fix's doc comments for what happens
// upstream once these land in the hub).
function throttledSliderSend(el, address, args) {
	let state = sliderSendState.get(el);
	if (!state) {
		state = { timer: null, lastSent: -Infinity, pending: null, dragUntil: -Infinity };
		sliderSendState.set(el, state);
	}
	state.pending = () => sendOSC(address, args);

	const now = performance.now();
	state.dragUntil = now + dragEchoGuardMs;
	const remaining = sendThrottleMs - (now - state.lastSent);
	if (remaining <= 0) {
		state.lastSent = now;
		state.pending();
		state.pending = null;
	} else if (!state.timer) {
		state.timer = setTimeout(() => {
			state.timer = null;
			state.lastSent = performance.now();
			state.pending();
			state.pending = null;
		}, remaining);
	}
}

function requestValues() {
	if (!selectedAux) return;
	const auxChannel = selectedAux.channel;
	for (const src of sources) {
		if (!src.enabled) continue;
		const root = addressRoot(src.type);
		sendOSC(root + "/" + src.number + "/Aux_Send/" + auxChannel + "/send_level/?");
		sendOSC(root + "/" + src.number + "/Aux_Send/" + auxChannel + "/send_pan/?");
	}
}

function sliderChange() {
	const el = this;
	let value = parseFloat(el.value);
	// Pan snaps to true center once close — a finger/mouse can drag near
	// 0.5 but rarely land exactly on it, and "almost centered" is a much
	// worse mix state than "not quite centered" would suggest, so this
	// rewrites both the input's own value (the thumb visibly jumps to
	// center) and what gets sent, not just the displayed fill.
	if (el.classList.contains("panInput") && Math.abs(value - 0.5) < panSnapThreshold) {
		value = 0.5;
		el.value = value;
	}
	el.parentNode.style.setProperty('--value', (value * 100) + "%");
	if (!ws || !selectedAux) return;

	const type = el.dataset.type, number = el.dataset.number;
	const root = addressRoot(type);
	if (el.classList.contains("volumeInput")) {
		throttledSliderSend(el, root + "/" + number + "/Aux_Send/" + selectedAux.channel + "/send_level", [sliderToDb(value)]);
	} else {
		throttledSliderSend(el, root + "/" + number + "/Aux_Send/" + selectedAux.channel + "/send_pan", [value]);
	}
}

function escapeHtml(s) {
	const d = document.createElement("div");
	d.textContent = s == null ? "" : s;
	return d.innerHTML;
}

// resetSliders zeroes every fader's on-screen position (not a live OSC
// send) — called right before requestValues() on an aux switch so stale
// values from the previous aux aren't briefly shown while fresh ones are
// in flight.
function resetSliders() {
	for (const el of channelsDiv.querySelectorAll(".volumeInput")) {
		el.value = 0;
		el.parentNode.style.setProperty('--value', '0%');
	}
	for (const el of channelsDiv.querySelectorAll(".panInput")) {
		el.value = 0.5;
		el.parentNode.style.setProperty('--value', '50%');
	}
}

// selectAux commits an aux as the active mix destination: persists it (per
// device, via localStorage — never synced elsewhere) so a returning client
// on this device skips straight past the picker next time, tints the page
// to the aux's colour, and shows the pan toggle only when this aux bus is
// itself stereo (panning within a mono bus is meaningless).
function selectAux(channel) {
	const aux = auxes.find((a) => String(a.channel) === String(channel));
	if (!aux) return;
	selectedAux = aux;
	localStorage.setItem("aux", String(channel));
	document.body.classList.remove("picking-aux");
	document.body.style.setProperty("--tint", hexToRgb(aux.colour) || defaultTint);
	auxIcon.src = iconSrc(aux.icon);
	auxLabel.textContent = aux.label || ("Aux " + aux.channel);
	panToggleWrap.hidden = !aux.stereo;
	panToggle.checked = false;
	document.body.classList.remove("panning");
	buildSources(layoutFor(aux));
	resetSliders();
	requestValues();
}

// buildAux renders the fullscreen aux picker and decides whether the
// client should land on it at all: a new client (or one whose saved aux no
// longer exists/is disabled — e.g. after a session change) has no valid
// saved choice and lands on the picker, mirroring the legacy app; a
// returning client on this device skips straight to its saved aux.
function buildAux(list) {
	auxes = list || [];
	let html = "";
	for (const a of auxes) {
		if (!a.enabled) continue;
		html += '<button type="button" class="aux-button" data-channel="' + a.channel + '" style="--tint:' + (hexToRgb(a.colour) || defaultTint) + '">';
		if (a.icon) html += '<img src="' + iconSrc(a.icon) + '" alt="" />';
		html += '<span>' + escapeHtml(a.label || ("Aux " + a.channel)) + '</span></button>';
	}
	auxPickerDiv.innerHTML = html;

	const saved = localStorage.getItem("aux");
	const stillValid = saved && auxes.some((a) => a.enabled && String(a.channel) === saved);
	if (stillValid) {
		selectAux(saved);
	} else {
		selectedAux = null;
		auxIcon.src = "";
		auxLabel.textContent = "Select aux…";
		panToggleWrap.hidden = true;
		buildSources(layoutFor(null));
		document.body.classList.add("picking-aux");
	}
}

// buildSources renders the mixer layout — a single ordered sequence mixing
// sources and admin-created section markers (see internal/oscmsg.LayoutItem's
// doc comment). sources (the module-level var) is filtered down to just the
// source-kind entries, since that's what requestValues()/sliderChange() need;
// the markers are only meaningful here, for inline headers at their exact
// position in the sequence — no separate lookup or sorting needed, the
// server already delivers this in final display order.
//
// Each source gets both a volume and a pan fader row, always present in the
// DOM — the pan-toggle just switches which one is visible (see mixer.css),
// so neither ever needs rebuilding when the toggle flips.
function buildSources(layout) {
	layout = layout || [];
	sources = layout.filter((it) => it.kind === "source");
	let html = "";
	for (const it of layout) {
		if (it.kind === "section") {
			html += '<h2>' + escapeHtml(it.sectionName) + '</h2>';
			continue;
		}
		const src = it;
		if (!src.enabled) continue;
		html += '<div class="source" data-key="' + sourceKey(src.type, src.number) + '">';
		html += '<span class="label">' + escapeHtml(src.label || (src.type + " " + src.number)) + '</span>';
		html += '<div class="fader-row volume-row"><input type="range" class="volumeInput" min="0" max="1" step="0.001" value="0" data-type="' + src.type + '" data-number="' + src.number + '" /></div>';
		html += '<div class="fader-row pan-row"><input type="range" class="panInput" min="0" max="1" step="0.001" value="0.5" data-type="' + src.type + '" data-number="' + src.number + '" /></div>';
		html += '</div>';
	}
	channelsDiv.innerHTML = html;
	for (const el of channelsDiv.querySelectorAll(".volumeInput, .panInput")) {
		el.addEventListener("input", sliderChange);
		el.parentNode.style.setProperty('--value', (parseFloat(el.value) * 100) + "%");
	}
}

function findSlider(type, number, cls) {
	return channelsDiv.querySelector('.' + cls + '[data-type="' + type + '"][data-number="' + number + '"]');
}

// applyStatus reflects the desk connectivity state machine (hub.Status) as
// one of four mutually exclusive body classes — kept distinct rather than
// collapsed into a single "offline" flag so starting/reloading (desk is
// fine, hub is mid-sequence) never gets confused with degraded (desk
// actually unreachable) in the UI, matching how the backend already
// distinguishes them. See mixer.css for what each class shows/hides.
function applyStatus(status) {
	const desk = (status && status.desk) || "";
	document.body.classList.toggle("status-starting", desk === "starting");
	document.body.classList.toggle("status-reloading", desk === "reloading");
	document.body.classList.toggle("status-degraded", desk === "degraded");
	document.body.classList.toggle("status-healthy", desk === "healthy");
	document.body.classList.toggle("locked", !!(status && status.locked));
}

const sendLevelPanRe = /^\/(Input_Channels|Group_Outputs)\/(\d+)\/Aux_Send\/(\d+)\/send_(level|pan)$/;
const nameRe = /^\/(Input_Channels|Group_Outputs)\/(\d+)\/(?:Channel_Input\/name|Buss_Trim\/name)$/;

function onMessage(e) {
	const env = JSON.parse(e.data);

	if (env.kind === "config") {
		snapshotSpan.textContent = (env.config && env.config.snapshot) || "";
		mixerLayouts = (env.config && env.config.mixerLayouts) || {};
		buildAux(env.config && env.config.auxes);
		requestValues();
		return;
	}

	if (env.kind === "status") {
		applyStatus(env.status);
		return;
	}

	if (env.kind !== "osc" || !env.message) return;
	const msg = env.message;

	if (msg.address === "/SnapshotName") {
		snapshotSpan.textContent = (msg.args && msg.args[0]) || "";
		return;
	}

	let m = sendLevelPanRe.exec(msg.address);
	if (m) {
		const type = m[1] === "Group_Outputs" ? "group" : "channel";
		const number = m[2], auxChannel = m[3], kind = m[4];
		if (!selectedAux || auxChannel !== String(selectedAux.channel)) return;
		const cls = kind === "level" ? "volumeInput" : "panInput";
		const el = findSlider(type, number, cls);
		if (!el || isDraggingLocally(el)) return;
		const value = kind === "level" ? dbToSlider(parseFloat(msg.args[0])) : parseFloat(msg.args[0]);
		el.value = value;
		el.parentNode.style.setProperty('--value', (value * 100) + "%");
		return;
	}

	m = nameRe.exec(msg.address);
	if (m) {
		const type = m[1] === "Group_Outputs" ? "group" : "channel";
		const el = channelsDiv.querySelector('.source[data-key="' + sourceKey(type, m[2]) + '"] .label');
		if (el) el.textContent = (msg.args && msg.args[0]) || "";
	}
}

function onOpen() {
	document.body.classList.remove("disconnected");
}

function noConnection() {
	if (ws) ws.close();
	clearTimeout(reconnectTimer);
	reconnectTimer = setTimeout(startWebsocket, 2000);
	document.body.classList.add("disconnected");
}

function startWebsocket() {
	ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
	ws.onopen = onOpen;
	ws.onmessage = onMessage;
	ws.onclose = noConnection;
	ws.onerror = noConnection;
}

document.addEventListener("DOMContentLoaded", function () {
	startWebsocket();

	auxTrigger.addEventListener("click", function () {
		document.body.classList.add("picking-aux");
	});

	auxPickerDiv.addEventListener("click", function (e) {
		const btn = e.target.closest(".aux-button");
		if (btn) selectAux(btn.dataset.channel);
	});

	panToggle.addEventListener("change", function () {
		document.body.classList.toggle("panning", this.checked);
	});
});

})();
