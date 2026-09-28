import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { installAuthExpiryInterceptor } from './lib/authExpiry'
import './index.css'

installAuthExpiryInterceptor(window)

ReactDOM.createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
)
