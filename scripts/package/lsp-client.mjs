// Bounded integration-test client. Each child owns a process group; cleanup
// runs after success, protocol failure, and request timeout.
import {spawn} from 'node:child_process';
export class Client {
 constructor(executable,args=[],options={}){
  this.next=1;this.pending=new Map();this.buffer=Buffer.alloc(0);this.stderr='';
  this.child=spawn(executable,args,{...options,detached:true,stdio:['pipe','pipe','pipe']});
  this.exited=new Promise(resolve=>{this.child.once('exit',(code,signal)=>{this.fail(Error(`Process exited: ${code}/${signal}: ${this.stderr}`));resolve();});this.child.once('error',error=>{this.fail(error);resolve();});});
  this.child.stdin.on('error',error=>this.fail(error));
  this.child.stderr.on('data',bytes=>{this.stderr=(this.stderr+bytes).slice(-65536);});
  this.child.stdout.on('data',bytes=>{
   try{
    this.buffer=Buffer.concat([this.buffer,bytes]);if(this.buffer.length>16*1024*1024)throw Error('Protocol frame exceeds 16 MiB');
    while(true){
     const end=this.buffer.indexOf('\r\n\r\n');if(end<0)return;
     const length=Number(/Content-Length: (\d+)/i.exec(this.buffer.subarray(0,end).toString())?.[1]);
     if(!Number.isSafeInteger(length)||length>16*1024*1024)throw Error('Invalid protocol length');
     if(this.buffer.length<end+4+length)return;
     const message=JSON.parse(this.buffer.subarray(end+4,end+4+length));this.buffer=this.buffer.subarray(end+4+length);
     if(message.method && message.id!==undefined){
      const result=message.method==='workspace/configuration'?(message.params?.items??[]).map(()=>({})):null;
      this.send({id:message.id,result});
     }else if(message.id!==undefined){const item=this.pending.get(message.id);if(item){this.pending.delete(message.id);clearTimeout(item.timer);message.error?item.reject(Error(JSON.stringify(message.error))):item.resolve(message.result);}}
    }
   }catch(error){this.fail(error);}
  });
 }
 fail(error){this.failure=error;for(const item of this.pending.values()){clearTimeout(item.timer);item.reject(error);}this.pending.clear();}
 send(message){const body=JSON.stringify({jsonrpc:'2.0',...message});this.child.stdin.write(`Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);}
 notify(method,params){this.send({method,params});}
 request(method,params,timeout=60000){
  if(this.failure)return Promise.reject(this.failure);
  const id=this.next++;return new Promise((resolve,reject)=>{
   const timer=setTimeout(()=>{this.pending.delete(id);reject(Error(`${method} timed out: ${this.stderr}`));},timeout);
   this.pending.set(id,{resolve,reject,timer});this.send({id,method,params});
  });
 }
 async close(){
  this.fail(Error('Client closed'));
  if(this.child.pid){try{process.kill(-this.child.pid,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}}
  let timer;try{await Promise.race([this.exited,new Promise((_,reject)=>{timer=setTimeout(()=>reject(Error('Process cleanup timed out')),5000);})]);}finally{clearTimeout(timer);}
 }
}
