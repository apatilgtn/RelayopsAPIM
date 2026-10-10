(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const signup = location.pathname === '/signup';
  let tokenMode = false;
  $('year').textContent = new Date().getFullYear();
  const message = (text, success = false) => {
    $('form-message').textContent = text;
    $('form-message').className = 'message' + (success ? ' success' : '');
    $('form-message').hidden = false;
  };
  if (signup) {
    document.title = 'Request access · RelayOps';
    $('form-title').textContent = 'Request workspace access';
    $('form-intro').textContent = 'Create an account for administrator review.';
    $('login-tab').removeAttribute('aria-current');
    $('signup-tab').setAttribute('aria-current', 'page');
    document.querySelectorAll('.signup-only').forEach(el => { el.hidden = false; });
    $('full-name').required = $('team').required = true;
    $('password').autocomplete = 'new-password';
    $('password').minLength = 12;
    $('password').placeholder = 'Create a strong password';
    $('password-help').textContent = 'At least 12 characters';
    $('submit').firstElementChild.textContent = 'Create account & request access';
    $('login-options').hidden = true;
  }
  $('show-password').onclick = () => {
    const visible = $('password').type === 'password';
    $('password').type = visible ? 'text' : 'password';
    $('show-password').textContent = visible ? 'Hide' : 'Show';
    $('show-password').setAttribute('aria-label', visible ? 'Hide password' : 'Show password');
    $('show-password').setAttribute('aria-pressed', String(visible));
  };
  $('token-mode').onclick = () => {
    tokenMode = !tokenMode;
    $('email-field').hidden = $('password-field').hidden = tokenMode;
    $('email').required = $('password').required = !tokenMode;
    $('token-field').hidden = !tokenMode;
    $('admin-token').required = tokenMode;
    $('submit').firstElementChild.textContent = tokenMode ? 'Sign in with administrator token' : 'Sign in to RelayOps';
    $('form-intro').textContent = tokenMode ? 'Use your platform administrator token. For your account password, switch back to email sign-in.' : 'Access your API workspace.';
    $('token-mode').textContent = tokenMode ? 'Use email and password instead' : 'Use an administrator token';
    $('form-message').hidden = true;
    (tokenMode ? $('admin-token') : $('email')).focus();
  };
  $('auth-form').onsubmit = async event => {
    event.preventDefault();
    if (!$('auth-form').reportValidity()) return;
    const button = $('submit');
    const label = button.firstElementChild.textContent;
    button.disabled = true;
    $('token-mode').disabled = true;
    button.firstElementChild.textContent = signup ? 'Submitting request…' : 'Signing in…';
    $('form-message').hidden = true;
    try {
      const body = signup ? {name:$('full-name').value.trim(), email:$('email').value.trim(), team:$('team').value.trim(), password:$('password').value}
        : tokenMode ? {token:$('admin-token').value.trim()} : {email:$('email').value.trim(), password:$('password').value};
      const response = await fetch(signup ? '/api/auth/signup' : '/api/auth/login', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)});
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(data.message || (response.status === 401 ? 'Check your credentials. New accounts also need administrator approval.' : 'Unable to continue. Please try again.'));
      $('password').value = $('admin-token').value = '';
      if (signup) {
        message(data.message || 'Your request is awaiting administrator approval.', true);
        $('form-title').textContent = 'Request received.';
        $('form-intro').textContent = 'Your administrator will review your workspace access.';
        $('auth-form').hidden = true;
        $('success-actions').hidden = false;
      } else {
        if (!data.token) throw new Error('The server did not return a sign-in session.');
        sessionStorage.setItem('relayops_token', data.token);
        localStorage.removeItem('relayops_token');
        location.replace('/#/live');
      }
    } catch (error) {
      message(error instanceof TypeError ? 'Could not reach RelayOps. Check your connection and try again.' : error.message);
    } finally { button.disabled = false; $('token-mode').disabled = false; button.firstElementChild.textContent = label; }
  };
})();
