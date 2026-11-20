"use strict";

document.addEventListener("submit", (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches(".pump-action-form, .mode-switch")) {
    return;
  }
  if (form.dataset.deviceControl === "true") {
    return;
  }
  if (form.dataset.submitting === "true") {
    event.preventDefault();
    return;
  }
  form.dataset.submitting = "true";
  form.setAttribute("aria-busy", "true");
  const submitter = event.submitter;
  for (const button of form.querySelectorAll('button[type="submit"]')) {
    button.disabled = true;
  }
  if (submitter instanceof HTMLButtonElement) {
    submitter.textContent = "Mengirim...";
  }
});

const appContent = document.getElementById("app-content");
const realtimePath = appContent && appContent.dataset.deviceWs;
if (realtimePath) {
  let reconnectDelay = 1000;
  let latestState = {
    sensor_terhubung: appContent.dataset.sensorConnected === "true",
    sensor_air_valid: appContent.dataset.sensorValid === "true",
    data_air_terbaru: appContent.dataset.sensorFresh === "true",
    koneksi_lora: appContent.dataset.loraOnline === "true",
  };
  const initialNode = appContent.dataset.initialNode;
  const initialRSSI = appContent.dataset.initialRssi;
  if (initialNode) {
    latestState.id_node = initialNode;
  }
  if (initialRSSI !== "" && Number.isFinite(Number(initialRSSI))) {
    latestState.sinyal_lora_dbm = Number(initialRSSI);
  }
  let latestUpdateMS = Number(appContent.dataset.lastUpdateMs || 0);
  const chartStartMS = Number(appContent.dataset.chartStartMs) || Date.now() - 24 * 60 * 60 * 1000;
  let observedPumpMode = appContent.dataset.pumpMode || "";
  let observedPumpOn = null;
  let statusPumpOn = null;
  let pendingControl = null;
  let pumpStatusTimestamp = 0;
  let autoOffDistance = Number.NaN;
  let autoOnDistance = Number.NaN;
  const initialAutoOff = document.getElementById("auto-off-distance");
  const initialAutoOn = document.getElementById("auto-on-distance");
  if (initialAutoOff && Number.isFinite(Number(initialAutoOff.textContent))) {
    autoOffDistance = Number(initialAutoOff.textContent);
  }
  if (initialAutoOn && Number.isFinite(Number(initialAutoOn.textContent))) {
    autoOnDistance = Number(initialAutoOn.textContent);
  }
  const initialPumpState = document.getElementById("pump-state-value");
  if (initialPumpState && initialPumpState.textContent.trim() !== "—") {
    observedPumpOn = initialPumpState.textContent.trim() === "AKTIF";
    statusPumpOn = observedPumpOn;
  }

  function setControlsDisabled(disabled) {
    const controlAvailable = appContent.dataset.deviceControlAvailable === "true";
    const modeForm = document.querySelector(".mode-switch[data-device-control='true']");
    if (modeForm) {
      for (const button of modeForm.querySelectorAll('button[type="submit"]')) {
        button.disabled = disabled || !controlAvailable;
      }
    }
    const pumpForm = document.querySelector(".pump-action-form[data-device-control='true']");
    if (pumpForm) {
      const pumpButton = pumpForm.querySelector('button[type="submit"]');
      if (pumpButton) {
        pumpButton.disabled = disabled || !controlAvailable || observedPumpMode !== "manual" ||
          appContent.dataset.pumpStatusAvailable !== "true" || typeof observedPumpOn !== "boolean";
      }
    }
  }

  function updatePumpControls() {
    const modeForm = document.querySelector(".mode-switch[data-device-control='true']");
    if (modeForm) {
      for (const button of modeForm.querySelectorAll('button[type="submit"]')) {
        const selected = normalizeMode(button.value) === observedPumpMode;
        button.classList.toggle("selected", selected);
        button.setAttribute("aria-pressed", selected ? "true" : "false");
      }
    }
    const button = document.getElementById("pump-toggle-button");
    if (button && typeof observedPumpOn === "boolean") {
      button.textContent = observedPumpOn ? "Matikan pompa" : "Nyalakan pompa";
      button.classList.toggle("button-danger", observedPumpOn);
      button.classList.toggle("button-primary", !observedPumpOn);
    }
    setControlsDisabled(pendingControl !== null);
  }

  function setControlStatus(message) {
    const status = document.getElementById("control-status");
    if (status) {
      status.textContent = message;
    }
  }

  async function submitDeviceControl(form, submitter) {
    if (pendingControl || form.dataset.submitting === "true") {
      return;
    }
    form.dataset.submitting = "true";
    form.setAttribute("aria-busy", "true");
    const formData = new FormData(form);
    if (submitter instanceof HTMLButtonElement && submitter.name) {
      formData.set(submitter.name, submitter.value);
    }
    setControlStatus("Mengirim perintah ke Firebase...");
    setControlsDisabled(true);
    try {
      const response = await fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(formData),
        headers: { Accept: "application/json" },
      });
      const result = await response.json();
      if (!response.ok) {
        throw new Error(result.error || "Perintah pompa gagal dikirim.");
      }
      pendingControl = result;
      pendingControl.startedAt = Date.now();
      appContent.dataset.desiredUpdatedAt = String(result.updatedAtMs);
      setControlStatus("Perintah terkirim. Menunggu konfirmasi status dari Firebase...");
      updatePumpControls();
    } catch (error) {
      form.dataset.submitting = "false";
      form.removeAttribute("aria-busy");
      setControlStatus(error instanceof Error ? error.message : "Perintah pompa gagal dikirim.");
      setControlsDisabled(false);
    }
  }

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement) || form.dataset.deviceControl !== "true" ||
        !form.matches(".pump-action-form, .mode-switch")) {
      return;
    }
    event.preventDefault();
    void submitDeviceControl(form, event.submitter);
  });

  function setText(id, value) {
    const element = document.getElementById(id);
    if (element) {
      element.textContent = value;
    }
  }

  function setHidden(id, hidden) {
    const element = document.getElementById(id);
    if (element) {
      element.hidden = hidden;
    }
  }

  function setMeter(id, barId, value, max) {
    const meter = document.getElementById(id);
    const bar = document.getElementById(barId);
    const current = Number.isFinite(value) && value >= 0 ? value : 0;
    if (meter) {
      meter.setAttribute("aria-valuenow", current.toFixed(1));
    }
    if (bar) {
      bar.style.width = `${Math.max(0, Math.min(100, current / max * 100))}%`;
    }
  }

  function setRealtimeConnection(connected) {
    document.querySelectorAll("[data-realtime-status]").forEach((element) => {
      element.textContent = connected ? "Realtime aktif" : "Realtime terputus; menghubungkan ulang";
    });
    document.querySelectorAll("[data-realtime-dot]").forEach((element) => {
      element.classList.toggle("offline", !connected);
    });
    if (connected) {
      setHidden("realtime-error", true);
    }
  }

  function formatUpdateTime(timestamp) {
    const parts = new Intl.DateTimeFormat("en-GB", {
      timeZone: "Asia/Makassar",
      day: "2-digit",
      month: "2-digit",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hourCycle: "h23",
    }).formatToParts(new Date(timestamp));
    const part = (type) => parts.find((item) => item.type === type)?.value || "";
    return `${part("day")}/${part("month")}/${part("year")} ${part("hour")}.${part("minute")}.${part("second")} WITA`;
  }

  function renderLastUpdateAge() {
    if (!Number.isFinite(latestUpdateMS) || latestUpdateMS <= 0) {
      return;
    }
    const age = Math.max(0, Date.now() - latestUpdateMS);
    let text;
    if (age < 60000) {
      text = age < 1000 ? "baru saja" : `${Math.floor(age / 1000)} detik lalu`;
    } else if (age < 3600000) {
      text = `${Math.floor(age / 60000)} menit lalu`;
    } else {
      text = `${Math.floor(age / 3600000)} jam lalu`;
    }
    document.querySelectorAll(".last-update-ago").forEach((element) => {
      element.textContent = text;
    });
    document.querySelectorAll(".last-update-time").forEach((element) => {
      element.textContent = formatUpdateTime(latestUpdateMS);
    });
    setHidden("device-stale-alert", age > 180000);
  }

  function updateLastUpdateFromTelemetry() {
    const timestamp = Number(latestState.waktu_update_server || latestState.waktu_terima_epoch_ms || 0);
    if (Number.isFinite(timestamp) && timestamp > latestUpdateMS) {
      latestUpdateMS = timestamp;
      appContent.dataset.lastUpdateMs = String(timestamp);
    }
    renderLastUpdateAge();
  }

  function updateAutoThresholds() {
    if (!Number.isFinite(autoOffDistance) || !Number.isFinite(autoOnDistance)) {
      return;
    }
    const note = document.getElementById("auto-threshold-note");
    if (note) {
      note.textContent = `Pompa otomatis mati saat jarak sensor ≤ ${autoOffDistance.toFixed(0)} cm dan menyala saat jarak ≥ ${autoOnDistance.toFixed(0)} cm. Garis menunjukkan batas pada tinggi air.`;
      note.hidden = document.getElementById("chart-wrap")?.hidden !== false;
    }
    const distance = latestState.jarak_sensor_ke_air_cm;
    const waterHeight = latestState.tinggi_air_sawah_cm;
    const valid = latestState.sensor_terhubung === true &&
      latestState.sensor_air_valid === true && latestState.data_air_terbaru === true &&
      Number.isFinite(distance) && Number.isFinite(waterHeight);
    if (!valid) {
      return;
    }
    const namespace = "http://www.w3.org/2000/svg";
    const chart = document.getElementById("water-chart");
    if (!chart) {
      return;
    }
    const referenceHeight = distance + waterHeight;
    const thresholds = [
      ["auto-off-threshold", "threshold-line threshold-off", referenceHeight - autoOffDistance],
      ["auto-on-threshold", "threshold-line threshold-on", referenceHeight - autoOnDistance],
    ];
    for (const [id, className, value] of thresholds) {
      if (!Number.isFinite(value) || value < 0 || value > 100) {
        continue;
      }
      let line = document.getElementById(id);
      if (!line) {
        line = document.createElementNS(namespace, "line");
        line.id = id;
        line.setAttribute("class", className);
        line.setAttribute("x1", "0");
        line.setAttribute("x2", "720");
        chart.insertBefore(line, document.getElementById("water-chart-line"));
      }
      const y = 10 + 140 * (1 - value / 100);
      line.setAttribute("y1", y.toFixed(1));
      line.setAttribute("y2", y.toFixed(1));
    }
  }

  function renderRecommendation() {
    const recommendation = document.getElementById("recommendation-text");
    if (!recommendation) {
      return;
    }
    const waterHeight = latestState.tinggi_air_sawah_cm;
    const heightText = Number.isFinite(waterHeight) && waterHeight >= 0
      ? `Tinggi air ${waterHeight.toFixed(1)} cm. `
      : "";
    if (observedPumpMode === "manual") {
      recommendation.textContent = `${heightText}Mode manual. Gunakan tombol untuk menyalakan atau mematikan pompa.`;
    } else if (Number.isFinite(autoOffDistance) && Number.isFinite(autoOnDistance)) {
      recommendation.textContent = `${heightText}Mode otomatis mengikuti ambang perangkat: pompa mati pada jarak ≤ ${autoOffDistance.toFixed(0)} cm dan menyala pada jarak ≥ ${autoOnDistance.toFixed(0)} cm.`;
    } else if (appContent.dataset.deviceLinked === "true") {
      recommendation.textContent = `${heightText}Pantau status perangkat dan sesuaikan mode pompa sesuai kondisi sawah.`;
    }
  }

  function renderLatest(changedFields = new Set()) {
    const connected = latestState.sensor_terhubung === true;
    const valid = connected && latestState.sensor_air_valid === true && latestState.data_air_terbaru === true;
    const distance = latestState.jarak_sensor_ke_air_cm;
    const waterHeight = latestState.tinggi_air_sawah_cm;
    const debit = latestState.debit_air_lpm;
    const sensorFieldsChanged = [
      "sensor_terhubung",
      "sensor_air_valid",
      "data_air_terbaru",
      "jarak_sensor_ke_air_cm",
      "tinggi_air_sawah_cm",
      "status_sensor_air",
    ].some((field) => changedFields.has(field));
    if (sensorFieldsChanged) {
      if (!valid) {
        if (!changedFields.has("jarak_sensor_ke_air_cm")) {
          latestState.jarak_sensor_ke_air_cm = null;
        }
        if (!changedFields.has("tinggi_air_sawah_cm")) {
          latestState.tinggi_air_sawah_cm = null;
        }
        setText("distance-value", "—");
        setText("water-height-value", "—");
        setMeter("water-level-meter", "water-level-meter-bar", 0, 100);
      } else {
        if (Number.isFinite(distance) && distance >= 0) {
          setText("distance-value", distance.toFixed(1));
        } else if (changedFields.has("jarak_sensor_ke_air_cm")) {
          setText("distance-value", "—");
        }
        if (Number.isFinite(waterHeight) && waterHeight >= 0) {
          setText("water-height-value", waterHeight.toFixed(1));
          setMeter("water-level-meter", "water-level-meter-bar", waterHeight, 100);
        } else {
          setText("water-height-value", "—");
          setMeter("water-level-meter", "water-level-meter-bar", 0, 100);
        }
      }
      const sensorStatus = String(latestState.status_sensor_air || "").trim();
      const statusIsOK = sensorStatus.toUpperCase() === "OK";
      const readiness = !connected
        ? (sensorStatus && !statusIsOK ? sensorStatus : "Sensor tidak terhubung")
        : latestState.sensor_air_valid !== true
          ? (sensorStatus && !statusIsOK ? sensorStatus : "Sensor tidak valid")
          : latestState.data_air_terbaru !== true
            ? (sensorStatus && !statusIsOK ? sensorStatus : "Data sensor belum baru")
            : sensorStatus && !statusIsOK
              ? sensorStatus
              : "Sensor siap";
      setText("sensor-readiness", readiness);
    }
    if (changedFields.has("debit_air_lpm")) {
      setText("flow-rate-value", Number.isFinite(debit) && debit >= 0 ? debit.toFixed(1) : "—");
      setMeter("flow-meter", "flow-meter-bar", debit, 120);
    }
    if (changedFields.has("flow_sudah_kalibrasi") && typeof latestState.flow_sudah_kalibrasi === "boolean") {
      setText("flow-calibration-status", latestState.flow_sudah_kalibrasi ? "Terkalibrasi" : "Belum dikalibrasi");
    }
    if (changedFields.has("total_volume_liter")) {
      const total = latestState.total_volume_liter;
      setText("total-liters-value", Number.isFinite(total) && total >= 0 ? ` · Total ${total.toFixed(1)} L` : "");
      setHidden("total-liters-value", !Number.isFinite(total) || total < 0);
    }
    if (changedFields.has("status_gangguan") && typeof latestState.status_gangguan === "string") {
      setText("device-fault-note", latestState.status_gangguan ? `Status sensor: ${latestState.status_gangguan}.` : "");
    }
    if (changedFields.has("keterangan_status") && typeof latestState.keterangan_status === "string") {
      setText("pump-status-reason", latestState.keterangan_status ? `Status pompa: ${latestState.keterangan_status}.` : "");
    }

    if (changedFields.has("pompa_menyala") && typeof latestState.pompa_menyala === "boolean") {
      if (typeof statusPumpOn !== "boolean") {
        observedPumpOn = latestState.pompa_menyala;
        appContent.dataset.pumpStatusAvailable = "true";
      }
      const pump = document.getElementById("pump-state-value");
      if (pump) {
        const pumpOn = typeof statusPumpOn === "boolean" ? statusPumpOn : latestState.pompa_menyala;
        pump.textContent = pumpOn ? "AKTIF" : "MATI";
        pump.classList.toggle("pump-on", pumpOn);
        pump.classList.toggle("pump-off", !pumpOn);
      }
      updatePumpControls();
    }
    if (changedFields.has("id_node") && typeof latestState.id_node === "string") {
      setText("device-node-value", latestState.id_node || "—");
    }
    if (changedFields.has("koneksi_lora") || changedFields.has("sinyal_lora_dbm")) {
      const online = latestState.koneksi_lora === true;
      const signal = Number.isFinite(latestState.sinyal_lora_dbm) ? ` · RSSI ${latestState.sinyal_lora_dbm} dBm` : "";
      setText("device-link-status", `${online ? "LoRa online" : "LoRa offline"}${signal}`);
    }
    updateAutoThresholds();
    renderRecommendation();
  }

  function applyRealtimeFields(message) {
    const update = message.data;
    if (update === undefined) {
      return;
    }
    const path = (message.path || "/").split("/").filter(Boolean);
    const changedFields = new Set();
    if (path.length === 0 && update && typeof update === "object" && !Array.isArray(update)) {
      Object.assign(latestState, update);
      Object.keys(update).forEach((field) => changedFields.add(field));
    } else if (path.length === 1) {
      latestState[path[0]] = update;
      changedFields.add(path[0]);
    }
    renderLatest(changedFields);
    updateLastUpdateFromTelemetry();
  }

  function renderPumpFields(message) {
    const update = message.data;
    if (update === undefined) {
      return;
    }
    const path = (message.path || "/").split("/").filter(Boolean);
    const fields = path.length === 0 && update && typeof update === "object" && !Array.isArray(update)
      ? update
      : path.length === 1
        ? { [path[0]]: update }
        : null;
    if (!fields) {
      return;
    }
    Object.assign(latestState, fields);
    if (typeof fields.pompa_menyala === "boolean") {
      statusPumpOn = fields.pompa_menyala;
      observedPumpOn = fields.pompa_menyala;
      appContent.dataset.pumpStatusAvailable = "true";
    }
    if (typeof fields.mode_operasi === "string") {
      observedPumpMode = normalizeMode(fields.mode_operasi);
      setText("pump-mode-value", `Mode ${observedPumpMode === "manual" ? "Manual" : observedPumpMode === "auto" ? "Otomatis" : fields.mode_operasi}`);
    }
    if (Number.isFinite(fields.batas_mati_otomatis_cm)) {
      autoOffDistance = fields.batas_mati_otomatis_cm;
    }
    if (Number.isFinite(fields.batas_nyala_otomatis_cm)) {
      autoOnDistance = fields.batas_nyala_otomatis_cm;
    }
    if (Number.isFinite(fields.waktu_update_server)) {
      pumpStatusTimestamp = Math.max(pumpStatusTimestamp, fields.waktu_update_server);
    }
    renderLatest(new Set(Object.keys(fields)));
    const desiredTimestamp = Number(appContent.dataset.desiredUpdatedAt || 0);
    if (appContent.dataset.deviceWriteAvailable === "true" &&
        desiredTimestamp > 0 && pumpStatusTimestamp > desiredTimestamp) {
      appContent.dataset.deviceControlAvailable = "true";
    }
    if (pendingControl && pumpStatusTimestamp > pendingControl.updatedAtMs) {
      const modeAcknowledged = observedPumpMode === pendingControl.expectedMode;
      const pumpAcknowledged = typeof pendingControl.expectedPumpOn !== "boolean" ||
        observedPumpOn === pendingControl.expectedPumpOn;
      if (modeAcknowledged && pumpAcknowledged) {
        pendingControl = null;
        document.querySelectorAll("[data-device-control='true']").forEach((form) => {
          form.dataset.submitting = "false";
          form.removeAttribute("aria-busy");
        });
        setControlStatus("Perangkat telah mengonfirmasi perintah.");
      }
    } else if (!pendingControl && fields.mode_operasi) {
      setControlStatus(observedPumpMode === "manual"
        ? "Mode manual: status relay mengikuti perintah dari aplikasi."
        : "Mode otomatis: pompa mengikuti batas otomatis perangkat.");
    }
    updatePumpControls();
  }

  function normalizeMode(mode) {
    const normalized = String(mode || "").trim().toUpperCase();
    if (normalized === "MANUAL") {
      return "manual";
    }
    if (normalized === "AUTO" || normalized === "OTOMATIS") {
      return "auto";
    }
    return "";
  }

  function addActivityRow(tbodyId, activity) {
    let tbody = document.getElementById(tbodyId);
    if (!tbody) {
      const card = document.querySelector(tbodyId === "history-rows" ? ".page-card" : ".activity-card");
      if (!card) {
        return false;
      }
      card.querySelector(".empty-state")?.remove();
      const wrapper = document.createElement("div");
      wrapper.className = "table-scroll";
      const table = document.createElement("table");
      const headings = tbodyId === "history-rows" ? ["Waktu", "Pembacaan", "Mode operasi"] : ["Waktu", "Kejadian", "Mode"];
      const thead = document.createElement("thead");
      const headerRow = document.createElement("tr");
      for (const heading of headings) {
        const th = document.createElement("th");
        th.textContent = heading;
        headerRow.append(th);
      }
      thead.append(headerRow);
      table.className = tbodyId === "history-rows" ? "history-table" : "activity-table";
      tbody = document.createElement("tbody");
      tbody.id = tbodyId;
      table.append(thead, tbody);
      wrapper.append(table);
      card.append(wrapper);
    }
    if (activity.key && [...tbody.querySelectorAll("tr[data-event-key]")].some((row) => row.dataset.eventKey === activity.key)) {
      return false;
    }
    const row = document.createElement("tr");
    if (activity.key) {
      row.dataset.eventKey = activity.key;
    }
    for (const value of [activity.time || "", activity.description || "", activity.mode || "—"]) {
      const cell = document.createElement("td");
      cell.textContent = value;
      row.append(cell);
    }
    const modeCell = row.cells[2];
    if (modeCell) {
      const tag = document.createElement("span");
      tag.className = "mode-tag";
      tag.textContent = activity.mode || "—";
      modeCell.replaceChildren(tag);
    }
    tbody.prepend(row);
    document.getElementById("activity-empty-state")?.remove();
    const maxRows = tbodyId === "history-rows" ? 50 : 5;
    while (tbody.rows.length > maxRows) {
      tbody.deleteRow(tbody.rows.length - 1);
    }
    return true;
  }

  function appendChartPoint(activity) {
    const line = document.getElementById("water-chart-line");
    if (!line || !activity.water_height_available || !Number.isFinite(activity.water_height_cm)) {
      return;
    }
    const timestamp = activity.timestamp_ms;
    if (!Number.isFinite(timestamp) || timestamp < chartStartMS || timestamp > chartStartMS + 24 * 60 * 60 * 1000) {
      return;
    }
    const x = Math.max(0, Math.min(720, ((timestamp - chartStartMS) / (24 * 60 * 60 * 1000)) * 720));
    const value = Math.max(0, Math.min(100, activity.water_height_cm));
    const y = 10 + 140 * (1 - value / 100);
    const points = (line.getAttribute("points") || "").trim().split(/\s+/).filter(Boolean);
    points.push(`${x.toFixed(1)},${y.toFixed(1)}`);
    line.setAttribute("points", points.slice(-50).join(" "));
    setHidden("chart-wrap", false);
    setHidden("chart-labels", false);
    setHidden("chart-empty-state", true);
    setHidden("auto-threshold-note", false);
    updateAutoThresholds();
  }

  function connect() {
    const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${window.location.host}${realtimePath}`);
    socket.addEventListener("open", () => {
      reconnectDelay = 1000;
      setRealtimeConnection(true);
    });
    socket.addEventListener("message", (event) => {
      let message;
      try {
        message = JSON.parse(event.data);
      } catch {
        return;
      }
      if (message.type === "telemetry") {
        applyRealtimeFields(message);
      } else if (message.type === "pump") {
        renderPumpFields(message);
      } else if (message.type === "history" && message.activity) {
        const page = Number(appContent.dataset.historyPage || 0);
        if (page === 0) {
          if (addActivityRow("activity-rows", message.activity)) {
            appendChartPoint(message.activity);
          }
        } else if (page === 1) {
          addActivityRow("history-rows", message.activity);
        }
      } else if (message.type === "error") {
        const error = document.getElementById("realtime-error");
        if (error) {
          error.textContent = message.message || "Koneksi realtime Firebase terputus.";
          error.hidden = false;
        }
      }
    });
    socket.addEventListener("close", () => {
      setRealtimeConnection(false);
      window.setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 30000);
    });
    socket.addEventListener("error", () => socket.close());
  }

  renderLastUpdateAge();
  window.setInterval(renderLastUpdateAge, 10000);
  setRealtimeConnection(false);
  connect();
}
