"use strict";

(() => {

const channelsDiv = document.getElementById('channels'),
auxSelect = document.getElementById('aux'),
snapshotSpan = document.getElementById('snapshot');

let ws = null,
reconnectTimer = null,
sources = [],
auxes = [];

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

function sendOSC(address, args) {
	if (!ws || ws.readyState !== WebSocket.OPEN) return;
	ws.send(JSON.stringify({
		kind: "osc",
		message: { address: address, args: args || [] }
	}));
}

function requestValues() {
	const auxChannel = auxSelect.value;
	if (!auxChannel) return;
	for (const src of sources) {
		if (!src.enabled) continue;
		const root = addressRoot(src.type);
		sendOSC(root + "/" + src.number + "/Aux_Send/" + auxChannel + "/send_level/?");
		sendOSC(root + "/" + src.number + "/Aux_Send/" + auxChannel + "/send_pan/?");
	}
}

function sliderChange() {
	const el = this;
	const value = parseFloat(el.value);
	el.parentNode.style.setProperty('--value', (value * 100) + "%");
	if (!ws) return;

	const type = el.dataset.type, number = el.dataset.number;
	const root = addressRoot(type);
	const auxChannel = auxSelect.value;
	if (!auxChannel) return;

	if (el.classList.contains("volumeInput")) {
		sendOSC(root + "/" + number + "/Aux_Send/" + auxChannel + "/send_level", [sliderToDb(value)]);
	} else {
		sendOSC(root + "/" + number + "/Aux_Send/" + auxChannel + "/send_pan", [value]);
	}
}

function escapeHtml(s) {
	const d = document.createElement("div");
	d.textContent = s == null ? "" : s;
	return d.innerHTML;
}

function buildAux(list) {
	auxes = list || [];
	let html = "";
	for (const a of auxes) {
		if (!a.enabled) continue;
		html += '<option value="' + a.channel + '" style="--tint:' + (a.colour || '#666') + '">' +
			escapeHtml(a.label || ("Aux " + a.channel)) + '</option>';
	}
	auxSelect.innerHTML = html;
	const saved = localStorage.getItem("aux");
	if (saved && auxes.some(a => String(a.channel) === saved)) {
		auxSelect.value = saved;
	}
}

// buildSources renders the mixer layout — a single ordered sequence mixing
// sources and admin-created section markers (see internal/oscmsg.LayoutItem's
// doc comment). sources (the module-level var) is filtered down to just the
// source-kind entries, since that's what requestValues()/sliderChange() need;
// the markers are only meaningful here, for inline headers at their exact
// position in the sequence — no separate lookup or sorting needed, the
// server already delivers this in final display order.
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
		html += '<input type="range" class="volumeInput" min="0" max="1" step="0.001" value="0" data-type="' + src.type + '" data-number="' + src.number + '" />';
		html += '<input type="range" class="panInput" min="0" max="1" step="0.001" value="0.5" data-type="' + src.type + '" data-number="' + src.number + '" />';
		html += '</div>';
	}
	channelsDiv.innerHTML = html;
	for (const el of channelsDiv.querySelectorAll(".volumeInput, .panInput")) {
		el.addEventListener("input", sliderChange);
	}
}

function findSlider(type, number, cls) {
	return channelsDiv.querySelector('.' + cls + '[data-type="' + type + '"][data-number="' + number + '"]');
}

const sendLevelPanRe = /^\/(Input_Channels|Group_Outputs)\/(\d+)\/Aux_Send\/(\d+)\/send_(level|pan)$/;
const nameRe = /^\/(Input_Channels|Group_Outputs)\/(\d+)\/(?:Channel_Input\/name|Buss_Trim\/name)$/;

function onMessage(e) {
	const env = JSON.parse(e.data);

	if (env.kind === "config") {
		snapshotSpan.textContent = (env.config && env.config.snapshot) || "";
		buildAux(env.config && env.config.auxes);
		buildSources(env.config && env.config.mixerLayout);
		requestValues();
		return;
	}

	if (env.kind === "status") {
		document.body.classList.toggle("desk-offline", env.status && env.status.desk !== "healthy");
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
		if (auxChannel !== auxSelect.value) return;
		const cls = kind === "level" ? "volumeInput" : "panInput";
		const el = findSlider(type, number, cls);
		if (!el) return;
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

	auxSelect.addEventListener("change", function () {
		localStorage.setItem("aux", this.value);
		for (const el of channelsDiv.querySelectorAll(".volumeInput")) {
			el.value = 0;
			el.parentNode.style.setProperty('--value', '0%');
		}
		for (const el of channelsDiv.querySelectorAll(".panInput")) {
			el.value = 0.5;
			el.parentNode.style.setProperty('--value', '50%');
		}
		requestValues();
	});
});

})();
