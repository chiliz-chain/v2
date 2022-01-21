import {observer} from "mobx-react"
import {Link, Route, Switch, useHistory} from "react-router-dom"
import {Menu, PageHeader} from "antd"
import {HomeOutlined, PropertySafetyOutlined} from "@ant-design/icons"
import {useState} from "react"

import HomePage from "./HomePage"

interface IIndexPageProps {
}

const titles: Record<string, any> = {
  home: {
    title: 'Home Page',
    sub: ' ',
  },
}

const IndexPage = observer((props: IIndexPageProps) => {
  const [currentPage, setCurrentPage] = useState('home')
  const history = useHistory()
  return <div style={{display: 'flex', height: '100%'}}>
    <div style={{width: '256px', height: '100%'}}>
      <Menu
        defaultSelectedKeys={['home']}
        style={{
          height: '100%'
        }}
        mode="inline"
        onClick={({key}) => {
          setCurrentPage(key)
        }}
        inlineCollapsed={false}
        theme="dark"
      >
        <Menu.Item key="home" icon={<HomeOutlined/>}>
          <Link to={'/'}>
            Consensus
          </Link>
        </Menu.Item>
      </Menu>
    </div>
    <div style={{width: '100%', padding: '0 20px 0'}}>
      <PageHeader
        className="site-page-header"
        onBack={() => history.goBack()}
        title={titles[currentPage].title || 'Unknown Page'}
        subTitle={titles[currentPage].sub || 'Unknown Page'}
      />
      <Switch>
        <Route path={"/"} component={HomePage}/>
      </Switch>
    </div>
  </div>
})

export default IndexPage
