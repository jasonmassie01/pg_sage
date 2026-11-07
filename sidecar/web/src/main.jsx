import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { RootErrorBoundary } from './components/PageErrorBoundary'
import { installAuthExpiryInterceptor } from './lib/authExpiry'
import './index.css'

installAuthExpiryInterceptor(window)

ReactDOM.createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <RootErrorBoundary>
      <App />
    </RootErrorBoundary>
  </React.StrictMode>
)
