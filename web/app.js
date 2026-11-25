'use strict';

const byId = (id) => document.getElementById(id);
const table = byId('rows');
const notice = byId('notice');
const filters = byId('filters');
const form = byId('magnet-form');
const picker = byId('torrent-file');
const confirmDialog = byId('confirm');
const nodes = new Map();
const pending = new Set();
const filterNames = {all: 'Все торренты', active: 'Активные', paused: 'На паузе', complete: 'Завершённые'};
const statuses = {
  metadata: 'Получение метаданных', waiting: 'Ожидание данных', paused: 'На паузе',
  downloading: 'Скачивается', complete: 'Раздаётся', error: 'Ошибка'
};
const units = ['Б', 'КиБ', 'МиБ', 'ГиБ', 'ТиБ', 'ПиБ'];
const number = new Intl.NumberFormat('ru', {maximumFractionDigits: 1});
let rows = [];
let filter = 'all';
let connected = false;
let polling = false;
let quitting = false;
let timer;

function bytes(value) {
  let scale = 0;
  while (value >= 1024 && scale < units.length - 1) {
    value /= 1024;
    scale++;
  }
  return number.format(value) + ' ' + units[scale];
}

function rate(value) {
  return bytes(value) + '/с';
}

function connection(online, label) {
  connected = online;
  byId('connection').textContent = label;
  byId('connection').parentElement.classList.toggle('online', online);
  for (const id of ['open-folder', 'quit', 'pick-file']) byId(id).disabled = !online;
  form.querySelector('button').disabled = !online || form.dataset.busy === 'true';
}

function message(text, error = false) {
  notice.textContent = text;
  notice.classList.toggle('error', error);
  notice.hidden = !text;
}

async function request(path, options = {}) {
  const response = await fetch('/api/' + path, {
    ...options, headers: {'X-Potok': '1', ...options.headers},
    signal: AbortSignal.timeout(20000)
  });
  if (!response.ok) {
    const error = await response.json().catch(() => ({}));
    throw new Error(error.error || 'Ошибка приложения (' + response.status + ').');
  }
  return response.json();
}

function matches(row, type) {
  if (type === 'paused') return row.paused;
  if (type === 'complete') return row.complete;
  if (type === 'active') return !row.paused && row.status !== 'error';
  return true;
}

function updateRow(node, row) {
  const percent = row.complete ? 100 : row.size > 0 ? Math.min(99.9, row.done / row.size * 100) : 0;
  const set = (selector, value) => {
    const field = node.querySelector(selector);
    if (field.textContent !== value) field.textContent = value;
  };
  set('.name-cell strong', row.name);
  set('.status', statuses[row.status]);
  node.querySelector('.status').className = 'status ' + row.status;
  set('.row-error', row.error || '');
  node.querySelector('.row-error').hidden = !row.error;
  set('.progress-label b', number.format(percent) + '%');
  set('.progress-label small', row.size || row.complete ? bytes(row.done) + ' / ' + bytes(row.size) : '—');
  const progress = node.querySelector('progress');
  progress.value = percent;
  progress.setAttribute('aria-label', 'Прогресс загрузки «' + row.name + '»');
  set('.speed-cell span', '↓ ' + rate(row.down));
  set('.speed-cell small', '↑ ' + rate(row.up));
  set('.peers-cell', String(row.peers));
  const toggle = node.querySelector('[data-action="toggle"]');
  const resume = row.paused || row.status === 'error';
  toggle.dataset.value = resume ? 'resume' : 'pause';
  set('[data-action="toggle"]', resume ? 'Продолжить' : 'Пауза');
  const busy = pending.has(row.id) || !connected;
  toggle.disabled = busy;
  node.querySelector('[data-action="folder"]').disabled = busy || !row.folder;
  node.querySelector('[data-action="remove"]').disabled = busy;
}

function render() {
  const selected = rows.filter((row) => matches(row, filter));
  for (const [id, node] of nodes) {
    if (!selected.some((row) => row.id === id)) {
      node.remove();
      nodes.delete(id);
    }
  }
  let cursor = table.firstElementChild;
  for (const row of selected) {
    let node = nodes.get(row.id);
    if (!node) {
      node = byId('row').content.firstElementChild.cloneNode(true);
      node.dataset.id = row.id;
      nodes.set(row.id, node);
    }
    updateRow(node, row);
    if (cursor !== node) table.insertBefore(node, cursor);
    cursor = node.nextElementSibling;
  }
  for (const type of Object.keys(filterNames)) {
    byId('count-' + type).textContent = rows.filter((row) => matches(row, type)).length;
  }
  byId('list-title').textContent = filterNames[filter];
  byId('down-total').textContent = rate(rows.reduce((total, row) => total + row.down, 0));
  byId('up-total').textContent = rate(rows.reduce((total, row) => total + row.up, 0));
  byId('table-wrap').hidden = selected.length === 0;
  byId('empty').hidden = selected.length !== 0;
  byId('empty-title').textContent = !connected ? 'Связь с клиентом потеряна' : rows.length ? 'Здесь пока пусто' : 'Торрентов пока нет';
  byId('empty-help').textContent = !connected ? 'Запустите Поток и откройте адрес из окна программы.'
    : rows.length ? 'Выберите другой фильтр, чтобы увидеть остальные торренты.'
    : 'Вставьте magnet-ссылку или откройте файл .torrent.';
}

async function poll() {
  if (polling || quitting) return;
  polling = true;
  try {
    const data = await request('queue');
    if (quitting) return;
    rows = data.rows;
    byId('directory').textContent = data.directory;
    byId('save-error').textContent = data.error || '';
    byId('save-error').hidden = !data.error;
    connection(true, 'Клиент запущен');
  } catch {
    if (quitting) return;
    connection(false, 'Нет связи с клиентом');
  } finally {
    polling = false;
    if (!quitting) render();
  }
}

async function add(body, type) {
  await request('torrents', {method: 'POST', headers: {'Content-Type': type}, body});
  filter = 'all';
  for (const button of filters.querySelectorAll('button')) {
    button.setAttribute('aria-pressed', String(button.dataset.filter === filter));
  }
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  if (!connected || form.dataset.busy === 'true') return;
  const input = byId('magnet');
  form.dataset.busy = 'true';
  form.querySelector('button').disabled = true;
  message('');
  try {
    await add(input.value.trim(), 'text/plain');
    input.value = '';
    message('Торрент добавлен.');
    await poll();
  } catch (error) {
    message(error.message, true);
  } finally {
    form.dataset.busy = 'false';
    form.querySelector('button').disabled = !connected;
  }
});

async function addFiles(files) {
  if (!connected) return;
  let loaded = 0;
  const errors = [];
  for (const file of files) {
    try {
      if (!file.name.toLowerCase().endsWith('.torrent')) throw new Error('Требуется файл .torrent.');
      await add(file, 'application/x-bittorrent');
      loaded++;
    } catch (error) {
      errors.push(file.name + ' — ' + error.message);
    }
  }
  const success = loaded ? 'Добавлено файлов: ' + loaded + '. ' : '';
  message(success + errors.join(' '), errors.length > 0);
  await poll();
}

byId('pick-file').addEventListener('click', () => picker.click());
picker.addEventListener('change', () => {
  const files = Array.from(picker.files);
  picker.value = '';
  if (files.length) addFiles(files);
});
const panel = document.querySelector('.add-panel');
window.addEventListener('dragover', (event) => {
  if (!event.dataTransfer.types.includes('Files')) return;
  event.preventDefault();
  event.dataTransfer.dropEffect = 'copy';
  panel.classList.add('dragging');
});
window.addEventListener('dragleave', (event) => {
  if (!event.relatedTarget) panel.classList.remove('dragging');
});
window.addEventListener('drop', (event) => {
  if (!event.dataTransfer.types.includes('Files')) return;
  event.preventDefault();
  panel.classList.remove('dragging');
  addFiles(Array.from(event.dataTransfer.files));
});

filters.addEventListener('click', (event) => {
  const button = event.target.closest('[data-filter]');
  if (!button) return;
  filter = button.dataset.filter;
  for (const option of filters.querySelectorAll('[data-filter]')) {
    option.setAttribute('aria-pressed', String(option === button));
  }
  render();
});

function confirmAction(title, description, label) {
  if (confirmDialog.open) return Promise.resolve(false);
  byId('confirm-title').textContent = title;
  byId('confirm-body').textContent = description;
  byId('confirm-button').textContent = label;
  confirmDialog.returnValue = '';
  confirmDialog.showModal();
  return new Promise((resolve) => {
    confirmDialog.addEventListener('close', () => resolve(confirmDialog.returnValue === 'confirm'), {once: true});
  });
}

table.addEventListener('click', async (event) => {
  const button = event.target.closest('[data-action]');
  if (!button) return;
  const id = button.closest('tr').dataset.id;
  const row = rows.find((entry) => entry.id === id);
  if (!row || pending.has(id)) return;
  const action = button.dataset.value || button.dataset.action;
  if (action === 'remove' && !await confirmAction('Убрать из очереди?', row.name + ' — скачанные файлы останутся на диске.', 'Убрать')) return;
  pending.add(id);
  render();
  try {
    await request('torrents/' + id + '/' + action, {method: 'POST'});
    if (action === 'remove') message('Торрент убран из очереди.');
    await poll();
  } catch (error) {
    message(error.message, true);
  } finally {
    pending.delete(id);
    render();
  }
});

byId('open-folder').addEventListener('click', async () => {
  try {
    await request('folder', {method: 'POST'});
  } catch (error) {
    message(error.message, true);
  }
});

byId('quit').addEventListener('click', async () => {
  if (!await confirmAction('Завершить работу?', 'Загрузки остановятся. Очередь сохранится для следующего запуска.', 'Завершить')) return;
  try {
    await request('quit', {method: 'POST'});
    quitting = true;
    clearInterval(timer);
    connection(false, 'Клиент закрыт');
    render();
    if (rows.length === 0) {
      byId('empty-title').textContent = 'Поток закрыт';
      byId('empty-help').textContent = '';
    }
    message('Работа завершена. Можно закрыть вкладку.');
  } catch (error) {
    message(error.message, true);
  }
});

connection(false, 'Подключение к клиенту');
poll();
timer = setInterval(poll, 1000);
