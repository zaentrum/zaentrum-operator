<#-- zaentrum demo login — self-contained template styled by zaentrum.css.
     Posts the standard username/password form to ${url.loginAction}; other
     flows fall back to the parent (keycloak.v2) theme via theme.properties. -->
<!DOCTYPE html>
<html lang="${(locale.currentLanguageTag)!'en'}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex, nofollow">
  <title>sign in · zaentrum</title>
  <link rel="icon" type="image/svg+xml" href="${url.resourcesPath}/img/favicon.svg">
  <link rel="stylesheet" href="${url.resourcesPath}/css/zaentrum.css">
</head>
<body>
  <main class="auth">
    <div class="card">
      <div class="brand" aria-label="zaentrum"><span class="gt">&gt;</span><span class="c">z</span></div>
      <h1>sign in</h1>
      <p class="sub">to continue to zaentrum</p>

      <#if message?? && (message.summary)?has_content>
        <div class="alert ${(message.type)!'info'}" role="alert">${kcSanitize(message.summary)?no_esc}</div>
      </#if>

      <form id="kc-form-login" action="${url.loginAction}" method="post" autocomplete="off" novalidate>
        <label for="username"><#if (realm.loginWithEmailAllowed)!false>Username or email<#else>Username</#if></label>
        <input id="username" name="username" type="text" autofocus autocomplete="username"
               value="${(login.username)!''}" dir="ltr">

        <label for="password">Password</label>
        <input id="password" name="password" type="password" autocomplete="current-password">

        <#if (realm.rememberMe)!false>
          <label class="remember">
            <input type="checkbox" name="rememberMe" <#if (login.rememberMe)??>checked</#if>> Remember me
          </label>
        </#if>

        <button type="submit" name="login" id="kc-login">sign in</button>
      </form>

      <#if (realm.resetPasswordAllowed)!false || (realm.registrationAllowed)!false>
        <div class="links">
          <#if (realm.resetPasswordAllowed)!false>
            <a class="link" href="${url.loginResetCredentialsUrl}">Forgot password?</a>
          <#else><span></span></#if>
          <#if (realm.registrationAllowed)!false>
            <a class="link" href="${url.registrationUrl}">Create account</a>
          </#if>
        </div>
      </#if>
    </div>
    <footer>zaentrum · demo</footer>
  </main>
</body>
</html>
