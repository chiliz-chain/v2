import {observer} from "mobx-react";
import {useState} from "react";
import {Button, Drawer, Menu} from "antd";
import {
  LockOutlined,
  PlusOutlined,
} from "@ant-design/icons";
import ProposalTable from "../components/ProposalTable";
import {useChilizStore} from "../stores";
import CreateProposalForm from "../components/CreateProposalForm";
import {Web3Address} from "../stores/ChilizStore";

const GovernanceNav = observer((props: any) => {
  const [drawerVisible, setDrawerVisible] = useState(false)
  const store = useChilizStore()
  return (
    <div>
      <Drawer
        title="Create proposal"
        width={500}
        onClose={() => {
          setDrawerVisible(false)
        }}
        bodyStyle={{paddingBottom: 80}}
        visible={drawerVisible}
      >
        <CreateProposalForm/>
      </Drawer>
      <ProposalTable/>
      <br/>
      <Button size={"large"} type={"primary"} onClick={() => {
        setDrawerVisible(true)
      }} icon={<PlusOutlined/>}>Create Proposal</Button>
      &nbsp;
      <Button size={"large"} onClick={async () => {
        const address = prompt('Input address: ')
        const result = await store.setVotingPower(address as Web3Address, '10000'),
          receipt = await result.receiptPromise
        console.log(result.transactionHash)
        console.log(receipt)
      }} icon={<PlusOutlined/>}>Set Voting Power</Button>
    </div>
  );
})

interface IHomePageProps {
}

const HomePage = observer((props: IHomePageProps) => {
  const store = useChilizStore()
  const [currentTab, setCurrentTab] = useState('governance')
  if (!store.isConnected) {
    return <h1>Connecting...</h1>
  }
  return (
    <div>
      <Menu
        selectedKeys={[currentTab]}
        onSelect={({selectedKeys}) => {
          setCurrentTab(selectedKeys[0])
        }}
        mode="horizontal"
      >
        <Menu.Item key="governance" icon={<LockOutlined/>}>
          Governance
        </Menu.Item>
      </Menu>
      <br/>
      {currentTab === 'governance' && <GovernanceNav/>}
    </div>
  );
});

export default HomePage
