const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('clusterheart', {
  login: (email, password) => ipcRenderer.invoke('auth:login', { email, password }),
  resume: () => ipcRenderer.invoke('auth:resume'),
  start: () => ipcRenderer.invoke('app:start'),
  logout: () => ipcRenderer.invoke('auth:logout'),
  openSignup: () => ipcRenderer.invoke('auth:signup-url'),
  openForgot: () => ipcRenderer.invoke('auth:forgot-url'),
});
