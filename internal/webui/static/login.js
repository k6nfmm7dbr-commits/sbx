/* SBX 登录页 —— 独立脚本（从 login.html 内联块移出，配合 CSP 收紧） */
'use strict';
(function () {
  if (location.search.indexOf('error') !== -1) {
    var e = document.getElementById('err');
    if (e) e.textContent = '令牌错误，请重试';
  }
})();
