// app.js — drives the admin page: load/save policy, poll status, OAuth links.

const ELIGIBILITY = ["everyone", "subscribers", "vips", "mods"];
const $ = (id) => document.getElementById(id);

function fillSelect(id) {
  const sel = $(id);
  sel.innerHTML = "";
  for (const v of ELIGIBILITY) {
    const o = document.createElement("option");
    o.value = v;
    o.textContent = v;
    sel.appendChild(o);
  }
}
["command_eligibility", "cp_eligibility", "sub_eligibility"].forEach(fillSelect);

// ---- policy form <-> object ----

function policyToForm(p) {
  $("g_cooldown").value = p.global.perUserCooldownSec;
  $("g_maxStream").value = p.global.maxPerStream;
  $("g_maxMinute").value = p.global.maxPerMinute;

  const s = p.sources;
  $("command_enabled").checked = s.command.enabled;
  $("command_trigger").value = s.command.trigger;
  $("command_eligibility").value = s.command.eligibility;

  $("cp_enabled").checked = s.channel_points.enabled;
  $("cp_rewardId").value = s.channel_points.rewardId || "";
  $("cp_eligibility").value = s.channel_points.eligibility;
  $("cp_cooldown").value = s.channel_points.perUserCooldownSec ?? "";

  $("sub_enabled").checked = s.subscription.enabled;
  $("sub_eligibility").value = s.subscription.eligibility;

  $("cheer_enabled").checked = s.cheer.enabled;
  $("cheer_minBits").value = s.cheer.minBits;

  $("raid_enabled").checked = s.raid.enabled;
  $("raid_minViewers").value = s.raid.minViewers;
}

function formToPolicy() {
  const cpCooldown = $("cp_cooldown").value.trim();
  const sources = {
    command: {
      enabled: $("command_enabled").checked,
      trigger: $("command_trigger").value.trim() || "!play",
      eligibility: $("command_eligibility").value,
    },
    channel_points: {
      enabled: $("cp_enabled").checked,
      rewardId: $("cp_rewardId").value.trim(),
      eligibility: $("cp_eligibility").value,
    },
    subscription: {
      enabled: $("sub_enabled").checked,
      eligibility: $("sub_eligibility").value,
    },
    cheer: {
      enabled: $("cheer_enabled").checked,
      minBits: Number($("cheer_minBits").value) || 0,
    },
    raid: {
      enabled: $("raid_enabled").checked,
      minViewers: Number($("raid_minViewers").value) || 0,
    },
  };
  if (cpCooldown !== "") sources.channel_points.perUserCooldownSec = Number(cpCooldown);
  return {
    global: {
      perUserCooldownSec: Number($("g_cooldown").value) || 0,
      maxPerStream: Number($("g_maxStream").value) || 1,
      maxPerMinute: Number($("g_maxMinute").value) || 1,
    },
    sources,
  };
}

async function loadPolicy() {
  const res = await fetch("/api/policy");
  policyToForm(await res.json());
}

$("savePolicy").addEventListener("click", async () => {
  const msg = $("saveMsg");
  msg.textContent = "Saving…";
  const res = await fetch("/api/policy", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(formToPolicy()),
  });
  if (res.ok) {
    msg.textContent = "Saved ✓";
    policyToForm(await res.json());
  } else {
    const err = await res.json().catch(() => ({}));
    msg.textContent = "Error: " + (err.error || res.status);
  }
  setTimeout(() => (msg.textContent = ""), 4000);
});

$("resetStream").addEventListener("click", async () => {
  await fetch("/api/reset-stream", { method: "POST" });
  refreshStatus();
});

// ---- delivery mode ----

$("saveDelivery").addEventListener("click", async () => {
  const msg = $("deliveryMsg");
  msg.textContent = "Saving…";
  const res = await fetch("/api/delivery", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode: $("delivery_mode").value }),
  });
  msg.textContent = res.ok ? "Saved ✓ — restart bot to apply" : "Error: " + res.status;
  setTimeout(() => (msg.textContent = ""), 5000);
});

// ---- status / dashboard ----

function connBlock(label, role, user) {
  if (user) return `<span class="ok">${label}: ${user.login} ✓</span>`;
  return `<a class="btn" href="/connect/${role}">Connect ${label}</a>`;
}

function timeAgo(ms) {
  const s = Math.round((Date.now() - ms) / 1000);
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  return Math.round(s / 3600) + "h ago";
}

async function refreshStatus() {
  let st;
  try {
    st = await (await fetch("/api/status")).json();
  } catch {
    return;
  }
  $("conn").innerHTML =
    connBlock("Broadcaster", "broadcaster", st.auth.broadcaster) +
    connBlock("Bot", "bot", st.auth.bot) +
    `<span class="${st.eventsub ? "ok" : "bad"}">EventSub: ${st.eventsub ? "connected" : "down"}</span>`;

  if (st.delivery) $("delivery_mode").value = st.delivery.mode;

  const s = st.stats;
  $("stats").innerHTML = [
    ["Codes in pool", st.poolSize],
    ["Granted this stream", st.streamGrants],
    ["Whispered", s.whispered],
    ["Public fallback", s.fallback],
    ["Chat", s.chat],
    ["Failed", s.failed],
    ["Denied (policy)", s.denied],
    ["Known users", st.totalUsers],
  ]
    .map(([k, v]) => `<div class="stat"><span>${v}</span><label>${k}</label></div>`)
    .join("");

  const tbody = $("recent").querySelector("tbody");
  tbody.innerHTML = st.recent
    .map(
      (e) =>
        `<tr><td>${timeAgo(e.at)}</td><td>${e.username}</td><td>${e.source}</td><td>${e.delivery}</td></tr>`,
    )
    .join("");
}

loadPolicy();
refreshStatus();
setInterval(refreshStatus, 5000);
