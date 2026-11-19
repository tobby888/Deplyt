for (const form of document.querySelectorAll('[data-deploy]')) {
  form.addEventListener('submit', () => {
    const button = form.querySelector('button');
    button.disabled = true;
    button.textContent = '正在提交…';
  });
}
