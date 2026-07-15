import urllib.request
r = urllib.request.urlopen('https://identidad.correos.es/static/js/main.d60fd9eb.chunk.js', timeout=30)
text = r.read().decode('utf-8', 'ignore')
for pat in ['PostAuthorizeAuthorize', 'GetUrlRedirectOauth', 'applicationOid', 'redirect_uri', 'Authorize']:
    print(pat, text.count(pat))
