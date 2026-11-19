const brandIcon = document.querySelector('#brand-icon');
const favicon = document.querySelector('link[rel="icon"]');

setInterval(() => {
  if (document.hidden) return;
  const url = '/brand-icon?v=' + Math.floor(Date.now() / 30000);
  brandIcon.src = url;
  favicon.href = url;
}, 30000);
