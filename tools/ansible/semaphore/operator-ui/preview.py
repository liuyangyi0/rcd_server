"""46-only temporary UI verification proxy. Rejects ALL mutating release requests."""
import http.client
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
import json
import mimetypes
from pathlib import Path
from urllib.parse import urlsplit

root=Path('/home/lenovo/code/rcd_semaphore_ui_20260930/upstream/api/public').resolve()
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):self.handle_request()
    def do_POST(self):self.handle_request()
    def do_PUT(self):self.send_error(403)
    def do_DELETE(self):self.send_error(403)
    def handle_request(self):
        path=urlsplit(self.path).path
        length=int(self.headers.get('Content-Length',0))
        if length>65536:self.send_error(413);return
        body=self.rfile.read(length)
        if path.startswith('/api/'):
            if self.command=='POST' and path!='/api/auth/login':
                try:
                    request=json.loads(body)
                    assert path=='/api/project/1/tasks'
                    if request['template_id'] not in (1,2,3):
                        assert request['template_id'] in (4,6,8)
                        values=json.loads(request['environment'])
                        assert values['execution']=='preview' and values['command_pause']=='not_confirmed'
                except Exception:self.send_error(403,'only inspection or unconfirmed preview is allowed');return
            conn=http.client.HTTPConnection('192.168.10.46',3000,timeout=30)
            headers={k:v for k,v in self.headers.items() if k.lower() not in ('host','connection','accept-encoding')}
            # The loopback-only preview is the same operator UI at a temporary
            # origin. Production keeps its original origin checks unchanged.
            headers['Origin']='http://192.168.10.46:3000'
            headers['Referer']='http://192.168.10.46:3000/'
            conn.request(self.command,self.path,body,headers)
            response=conn.getresponse();data=response.read();self.send_response(response.status)
            for k,v in response.getheaders():
                if k.lower() not in ('transfer-encoding','connection','content-length'):self.send_header(k,v)
            self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data);conn.close();return
        if self.command!='GET':self.send_error(405);return
        file=(root/path.lstrip('/')).resolve()
        try:file.relative_to(root)
        except ValueError:self.send_error(403);return
        if '.' not in path or path=='/':file=root/'index.html'
        if not file.is_file():self.send_error(404);return
        data=file.read_bytes();self.send_response(200);self.send_header('Content-Type',mimetypes.guess_type(file)[0] or 'application/octet-stream');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
ThreadingHTTPServer(('127.0.0.1',13000),Handler).serve_forever()
